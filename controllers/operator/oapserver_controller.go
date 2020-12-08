// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package controllers

import (
	"context"
	"fmt"
	"text/template"
	"time"

	"github.com/go-logr/logr"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apiequal "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/apache/skywalking-swck/apis/operator/v1alpha1"
	"github.com/apache/skywalking-swck/pkg/kubernetes"
)

const annotationKeyIstioSetup = "istio-setup-command"

var schedDuration, _ = time.ParseDuration("1m")
var rushModeSchedDuration, _ = time.ParseDuration("5s")

// OAPServerReconciler reconciles a OAPServer object
type OAPServerReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	FileRepo kubernetes.Repo
}

// +kubebuilder:rbac:groups=operator.skywalking.apache.org,resources=oapservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.skywalking.apache.org,resources=oapservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *OAPServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("oapserver", req.NamespacedName)
	log.Info("=====================reconcile started================================")

	oapServer := operatorv1alpha1.OAPServer{}
	if err := r.Client.Get(ctx, req.NamespacedName, &oapServer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ff, err := r.FileRepo.GetFilesRecursive("templates")
	if err != nil {
		log.Error(err, "failed to load resource templates")
		return ctrl.Result{}, err
	}
	app := kubernetes.Application{
		Client:   r.Client,
		FileRepo: r.FileRepo,
		CR:       &oapServer,
		GVK:      operatorv1alpha1.GroupVersion.WithKind("OAPServer"),
		TmplFunc: tmplFunc(&oapServer),
	}
	for _, f := range ff {
		l := log.WithName(f)
		if err := app.Apply(ctx, f, l); err != nil {
			l.Error(err, "failed to apply resource")
			return ctrl.Result{}, err
		}
	}

	r.istio(ctx, log, oapServer.Name, &oapServer)

	return ctrl.Result{RequeueAfter: r.checkState(ctx, log, &oapServer)}, nil
}

func tmplFunc(oapServer *operatorv1alpha1.OAPServer) template.FuncMap {
	return template.FuncMap{
		"generateImage": func() string {
			image := oapServer.Spec.Image
			if image == "" {
				v := oapServer.Spec.Version
				vTmpl := "apache/skywalking-oap-server:%s-%s"
				vES := "es6"
				for _, e := range oapServer.Spec.Config {
					if e.Name != "SW_STORAGE" {
						continue
					}
					if e.Value == "elasticsearch7" {
						vES = "es7"
					}
				}
				image = fmt.Sprintf(vTmpl, v, vES)
			}
			return image
		},
	}
}

func (r *OAPServerReconciler) checkState(ctx context.Context, log logr.Logger, oapServer *operatorv1alpha1.OAPServer) time.Duration {
	overlay := operatorv1alpha1.OAPServerStatus{}
	deployment := apps.Deployment{}
	nextSchedule := schedDuration
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: oapServer.Namespace, Name: oapServer.Name}, &deployment); err != nil {
		nextSchedule = rushModeSchedDuration
	} else {
		overlay.Conditions = deployment.Status.Conditions
		overlay.AvailableReplicas = deployment.Status.AvailableReplicas
		if oapServer.Spec.Instances != overlay.AvailableReplicas {
			nextSchedule = rushModeSchedDuration
		}
		if oapServer.Spec.Image != deployment.Spec.Template.Spec.Containers[0].Image {
			oapServer.Spec.Image = deployment.Spec.Template.Spec.Containers[0].Image
			if err := r.Update(ctx, oapServer); err != nil {
				log.Error(err, "failed to update OAPServer Image field")
			}
			log.Info("updated OAPServer Image")
			nextSchedule = rushModeSchedDuration
		}
	}
	service := core.Service{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: oapServer.Namespace, Name: oapServer.Name}, &service); err != nil {
		nextSchedule = rushModeSchedDuration
	} else {
		overlay.Address = fmt.Sprintf("%s.%s", service.Name, service.Namespace)
	}
	if apiequal.Semantic.DeepDerivative(overlay, oapServer.Status) {
		log.Info("Status keeps the same as before")
		return nextSchedule
	}
	oapServer.Status = overlay
	if err := kubernetes.ApplyOverlay(oapServer, &operatorv1alpha1.OAPServer{Status: overlay}); err != nil {
		log.Error(err, "failed to overlay OAPServer")
		return rushModeSchedDuration
	}
	if err := r.Status().Update(ctx, oapServer); err != nil {
		return rushModeSchedDuration
	}
	log.Info("updated Status sub resource")
	return nextSchedule
}

func (r *OAPServerReconciler) istio(ctx context.Context, log logr.Logger, serviceName string, oapServer *operatorv1alpha1.OAPServer) {
	for _, envVar := range oapServer.Spec.Config {
		if envVar.Name == "SW_ENVOY_METRIC_ALS_HTTP_ANALYSIS" &&
			oapServer.ObjectMeta.Annotations[annotationKeyIstioSetup] == "" {
			oapServer.Annotations[annotationKeyIstioSetup] = fmt.Sprintf("istioctl install --set profile=demo "+
				"--set meshConfig.defaultConfig.envoyAccessLogService.address=%s.%s:11800 "+
				"--set meshConfig.enableEnvoyAccessLogService=true", serviceName, oapServer.Namespace)
			if err := r.Update(ctx, oapServer); err != nil {
				log.Error(err, "unable to patch Istio setup command to annotation")
				return
			}
			log.Info("patched Istio annotation")
			return
		}
	}
}

func (r *OAPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&operatorv1alpha1.OAPServer{}).
		Complete(r)
}