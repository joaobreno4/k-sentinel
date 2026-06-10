/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/joaobreno4/k-sentinel/pkg/observability"
)

const (
	annotationMonitor = "kubeobserver.io/monitor"
	annotationTeam    = "kubeobserver.io/team"
	finalizerName     = "kubeobserver.io/finalizer"
)

// DeploymentReconciler reconciles a Deployment object.
// ObservabilityClient is injected at startup — swap the concrete type in
// cmd/main.go to target a different monitoring backend.
type DeploymentReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	ObservabilityClient observability.MonitorClient
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
// +kubebuilder:rbac:groups=apps,resources=deployments/finalizers,verbs=update

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// --- 1. Fetch ---
	// Retrieve the current state of the Deployment from the API server.
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			// The object was deleted before this reconcile ran; no action needed.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// --- 2. Annotation filter ---
	// Ignore any Deployment that does not opt into monitoring. Returning nil
	// here means "success, do not requeue" — the controller will only act
	// again if a new event arrives for this object.
	if deployment.Annotations[annotationMonitor] != "true" {
		return ctrl.Result{}, nil
	}

	// --- 3. Deletion flow ---
	// A non-zero DeletionTimestamp means kubectl delete was called but the
	// API server is waiting for our finalizer to be removed before it
	// garbage-collects the object. We own the external monitor lifecycle,
	// so we must clean up before releasing the object.
	if !deployment.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, deployment)
	}

	// --- 4. Creation / update flow ---
	return r.reconcileUpsert(ctx, deployment)
}

// reconcileUpsert handles the "live" state: ensures our finalizer is registered
// and that the external monitors reflect the current Deployment.
func (r *DeploymentReconciler) reconcileUpsert(ctx context.Context, deployment *appsv1.Deployment) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Register our finalizer before calling the external API. If the process
	// crashes between the two steps, the finalizer ensures the next reconcile
	// will still attempt deletion. After Update, the API server emits a new
	// watch event which re-enters Reconcile — we return early so CreateMonitors
	// is only called once the finalizer is durably persisted in etcd.
	if !controllerutil.ContainsFinalizer(deployment, finalizerName) {
		controllerutil.AddFinalizer(deployment, finalizerName)
		if err := r.Update(ctx, deployment); err != nil {
			log.Error(err, "Failed to register finalizer")
			return ctrl.Result{}, err
		}
		log.Info("Finalizer registered", "deployment", deployment.Name, "namespace", deployment.Namespace)
		return ctrl.Result{}, nil
	}

	team := deployment.Annotations[annotationTeam]
	log.Info("Ensuring monitors", "deployment", deployment.Name, "namespace", deployment.Namespace, "team", team)

	if err := r.ObservabilityClient.CreateMonitors(ctx, deployment.Name, team); err != nil {
		log.Error(err, "Failed to create monitors in Datadog",
			"deployment", deployment.Name, "namespace", deployment.Namespace)
		return ctrl.Result{}, err
	}

	log.Info("Monitors ensured", "deployment", deployment.Name, "namespace", deployment.Namespace)
	return ctrl.Result{}, nil
}

// reconcileDelete cleans up external monitors and removes our finalizer so the
// API server can proceed with garbage-collecting the Deployment.
func (r *DeploymentReconciler) reconcileDelete(ctx context.Context, deployment *appsv1.Deployment) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(deployment, finalizerName) {
		// Finalizer already gone — nothing for us to do.
		return ctrl.Result{}, nil
	}

	log.Info("Deleting monitors before allowing Deployment removal",
		"deployment", deployment.Name, "namespace", deployment.Namespace)

	// We MUST NOT remove the finalizer if DeleteMonitors fails. Returning the
	// error here causes controller-runtime to requeue with exponential backoff,
	// keeping the Deployment in a terminating state until cleanup succeeds.
	// Releasing the finalizer on failure would orphan monitors in Datadog.
	if err := r.ObservabilityClient.DeleteMonitors(ctx, deployment.Name); err != nil {
		log.Error(err, "Failed to delete monitors from Datadog",
			"deployment", deployment.Name, "namespace", deployment.Namespace)
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(deployment, finalizerName)
	if err := r.Update(ctx, deployment); err != nil {
		log.Error(err, "Failed to remove finalizer after cleanup")
		return ctrl.Result{}, err
	}

	log.Info("Monitors deleted and finalizer removed, Deployment can now be garbage-collected",
		"deployment", deployment.Name, "namespace", deployment.Namespace)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Named("deployment").
		Complete(r)
}
