// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package controller

import (
	"context"
	"errors"

	"github.com/aws/symphony/api/v1alpha1"
	"github.com/aws/symphony/internal/condition"
	"github.com/aws/symphony/internal/crd"
	serr "github.com/aws/symphony/internal/errors"
	"github.com/aws/symphony/internal/requeue"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// handleReconcileError will handle errors from reconcile handlers, which
// respects runtime errors.
func (r *ResourceGroupReconciler) handleReconcileError(ctx context.Context, err error) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var requeueNeededAfter *requeue.RequeueNeededAfter
	if errors.As(err, &requeueNeededAfter) {
		after := requeueNeededAfter.Duration()
		log.Info(
			"requeue needed after error",
			"error", requeueNeededAfter.Unwrap(),
			"after", after,
		)
		return ctrl.Result{RequeueAfter: after}, nil
	}

	var requeueNeeded *requeue.RequeueNeeded
	if errors.As(err, &requeueNeeded) {
		log.Info(
			"requeue needed error",
			"error", requeueNeeded.Unwrap(),
		)
		return ctrl.Result{Requeue: true}, nil
	}

	var noRequeue *requeue.NoRequeue
	if errors.As(err, &noRequeue) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, err
}

func (r *ResourceGroupReconciler) setResourceGroupStatus(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup, topologicalOrder []string, reconcileErr error) error {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("calculating resource group status and conditions")

	dc := resourcegroup.DeepCopy()

	// set conditions
	dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
		condition.NewReconcilerReadyCondition(corev1.ConditionTrue, "", "micro controller is ready"),
	)
	dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
		condition.NewGraphVerifiedCondition(corev1.ConditionTrue, "", "Directed Acyclic Graph is synced"),
	)
	dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
		condition.NewCustomResourceDefinitionSyncedCondition(corev1.ConditionTrue, "", "Custom Resource Definition is synced"),
	)
	dc.Status.State = "ACTIVE"
	dc.Status.TopoligicalOrder = topologicalOrder

	if reconcileErr != nil {
		log.V(1).Info("Error occurred during reconcile", "error", reconcileErr)

		var processCRDErr *serr.ProcessCRDError
		if errors.As(reconcileErr, &processCRDErr) {
			log.V(1).Info("Handling CRD (open-simple-schema) error", "error", reconcileErr)
			// set all conditions to unknown and crd condition to false
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(corev1.ConditionUnknown, "error parsing schema: "+reconcileErr.Error(), "Directed Acyclic Graph is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(corev1.ConditionFalse, "error parsing schema: "+reconcileErr.Error(), "Custom Resource Definition is synced"),
			)
			reason := "Faulty Graph"
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(corev1.ConditionUnknown, reason, "micro controller is ready"),
			)
		}

		// if the error is graph error, graph condition should be false and the rest should be unknown
		var reconcielGraphErr *serr.ReconcileGraphError
		if errors.As(reconcileErr, &reconcielGraphErr) {
			log.V(1).Info("Processing reconcile graph error", "error", reconcileErr)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(corev1.ConditionFalse, reconcileErr.Error(), "Directed Acyclic Graph is synced"),
			)

			reason := "Faulty Graph"
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(corev1.ConditionUnknown, reason, "micro controller is ready"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(corev1.ConditionUnknown, reason, "Custom Resource Definition is synced"),
			)
		}

		// if the error is crd error, crd condition should be false, graph condition should be true and the rest should be unknown
		var reconcileCRDErr *serr.ReconcileCRDError
		if errors.As(reconcileErr, &reconcileCRDErr) {
			log.V(1).Info("Processing reconcile crd error", "error", reconcileErr)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(corev1.ConditionTrue, "", "Directed Acyclic Graph is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(corev1.ConditionFalse, reconcileErr.Error(), "Custom Resource Definition is synced"),
			)
			reason := "CRD not-synced"
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(corev1.ConditionUnknown, reason, "micro controller is ready"),
			)
		}

		// if the error is micro controller error, micro controller condition should be false, graph condition should be true and the rest should be unknown
		var reconcileMicroController *serr.ReconcileMicroControllerError
		if errors.As(reconcileErr, &reconcileMicroController) {
			log.V(1).Info("Processing reconcile micro controller error", "error", reconcileErr)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(corev1.ConditionTrue, "", "Directed Acyclic Graph is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(corev1.ConditionTrue, "", "Custom Resource Definition is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(corev1.ConditionFalse, reconcileErr.Error(), "micro controller is ready"),
			)
		}

		log.V(1).Info("Setting resource group status to INACTIVE", "error", reconcileErr)
		dc.Status.State = "INACTIVE"
	}

	log.V(1).Info("Setting resource group status", "status", dc.Status)
	patch := client.MergeFrom(resourcegroup.DeepCopy())
	return r.Status().Patch(ctx, dc.DeepCopy(), patch)
}

func (r *ResourceGroupReconciler) setManaged(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup) error {
	log := log.FromContext(ctx)
	log.V(1).Info("setting resourcegroup as managed - adding finalizer")

	newFinalizers := []string{v1alpha1.SymphonyDomainName}
	dc := resourcegroup.DeepCopy()
	dc.Finalizers = newFinalizers
	if len(dc.Finalizers) != len(resourcegroup.Finalizers) {
		patch := client.MergeFrom(resourcegroup.DeepCopy())
		return r.Patch(ctx, dc.DeepCopy(), patch)
	}
	return nil
}

func (r *ResourceGroupReconciler) setUnmanaged(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup) error {
	log := log.FromContext(ctx)
	log.V(1).Info("setting resourcegroup as unmanaged - removing finalizer")

	newFinalizers := []string{}
	dc := resourcegroup.DeepCopy()
	dc.Finalizers = newFinalizers
	patch := client.MergeFrom(resourcegroup.DeepCopy())
	return r.Patch(ctx, dc.DeepCopy(), patch)
}

func getGVR(customRD *v1.CustomResourceDefinition) *schema.GroupVersionResource {
	return &schema.GroupVersionResource{
		Group: customRD.Spec.Group,
		// Deal with complex versioning later on
		Version:  customRD.Spec.Versions[0].Name,
		Resource: customRD.Spec.Names.Plural,
	}
}

func processCRD(ctx context.Context, resourceGroup *v1alpha1.ResourceGroup) (*v1.CustomResourceDefinition, *schema.GroupVersionResource, error) {
	customCRD, err := crd.BuildCRDObjectFromRawNeoCRDSchema(resourceGroup.Spec.APIVersion, resourceGroup.Spec.Kind, resourceGroup.Spec.Definition)
	if err != nil {
		return nil, nil, serr.NewProcessCRDError(err)
	}
	gvr := getGVR(customCRD)
	return customCRD, gvr, nil
}