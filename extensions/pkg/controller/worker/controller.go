// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"

	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	extensionspredicate "github.com/gardener/gardener/extensions/pkg/predicate"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
)

const (
	// FinalizerName is the worker controller finalizer.
	FinalizerName = "extensions.gardener.cloud/worker"
	// ControllerName is the name of the controller.
	ControllerName = "worker"
)

// AddArgs are arguments for adding a Worker controller to a manager.
type AddArgs struct {
	// Actuator is a Worker actuator.
	Actuator Actuator
	// ControllerOptions are the controller options used for creating a controller.
	// The options.Reconciler is always overridden with a reconciler created from the
	// given actuator.
	ControllerOptions controller.Options
	// Predicates are the predicates to use.
	// If unset, GenerationChangedPredicate will be used.
	Predicates []predicate.Predicate
	// Type is the type of the resource considered for reconciliation.
	Type string
	// IgnoreOperationAnnotation specifies whether to ignore the operation annotation or not.
	// If the annotation is not ignored, the extension controller will only reconcile
	// with a present operation annotation typically set during a reconcile (e.g. in the maintenance time) by the Gardenlet
	IgnoreOperationAnnotation bool
	// ExtensionClass defines the extension class this extension is responsible for.
	ExtensionClass extensionsv1alpha1.ExtensionClass
}

// DefaultPredicates returns the default predicates for a Worker reconciler.
func DefaultPredicates(ctx context.Context, mgr manager.Manager, ignoreOperationAnnotation bool) []predicate.Predicate {
	return extensionspredicate.DefaultControllerPredicates(ignoreOperationAnnotation, extensionspredicate.ShootNotFailedPredicate(ctx, mgr))
}

// Add creates a new Worker Controller and adds it to the Manager.
// and Start it when the Manager is Started.
func Add(ctx context.Context, mgr manager.Manager, args AddArgs) error {
	predicates := extensionspredicate.AddTypeAndClassPredicates(args.Predicates, args.ExtensionClass, args.Type)

	c, err := builder.
		ControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(args.ControllerOptions).
		Watches(
			&extensionsv1alpha1.Worker{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicates...),
		).
		WatchesRawSource(source.Kind[client.Object](
			mgr.GetCache(),
			&machinev1alpha1.Machine{},
			handler.EnqueueRequestsFromMapFunc(MachineToWorkerMapper()),
			MachineConditionChangedPredicate(ctx, mgr.GetLogger().WithValues("controller", ControllerName), mgr.GetClient()),
		)).
		Build(NewReconciler(mgr, args.Actuator))
	if err != nil {
		return err
	}

	if args.IgnoreOperationAnnotation {
		if err := c.Watch(source.Kind[client.Object](
			mgr.GetCache(),
			&extensionsv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(ClusterToWorkerMapper(mgr.GetClient(), predicates)),
		)); err != nil {
			return err
		}
	}

	return nil
}

// MachineConditionChangedPredicate returns a predicate function that returns
// - true for Create events if the MachineDeployment strategy is InPlaceUpdate and OrchestrationType is Manual
// - true for Update events if the MachineDeployment strategy is InPlaceUpdate and OrchestrationType is Manual and machine condition transitioned from UpdateCandidate to SelectedForUpdate
// - false for Delete and Generic events
func MachineConditionChangedPredicate(ctx context.Context, log logr.Logger, c client.Client) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			machine, ok := e.Object.(*machinev1alpha1.Machine)
			if !ok {
				return false
			}

			machineDeploymentName, ok := machine.Labels[LabelKeyMachineDeploymentName]
			if !ok {
				return false
			}

			machineDeployment := &machinev1alpha1.MachineDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineDeploymentName,
					Namespace: machine.Namespace,
				},
			}

			if err := c.Get(ctx, client.ObjectKeyFromObject(machineDeployment), machineDeployment); err != nil {
				log.Error(err, "Failed to get machine deployment for machine", "machine", machine.Name, "machineDeployment", machineDeploymentName)
				return false
			}

			return gardenerutils.IsMachineDeploymentStrategyManualInPlace(machineDeployment.Spec.Strategy)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, ok := e.ObjectOld.(*machinev1alpha1.Machine)
			if !ok {
				return false
			}

			newMachine, ok := e.ObjectNew.(*machinev1alpha1.Machine)
			if !ok {
				return false
			}

			machineDeploymentName, ok := newMachine.Labels[LabelKeyMachineDeploymentName]
			if !ok {
				return false
			}

			machineDeployment := &machinev1alpha1.MachineDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      machineDeploymentName,
					Namespace: newMachine.Namespace,
				},
			}

			if err := c.Get(ctx, client.ObjectKeyFromObject(machineDeployment), machineDeployment); err != nil {
				log.Error(err, "Failed to get machine deployment for machine", "machine", newMachine.Name, "machineDeployment", machineDeploymentName)
				return false
			}

			// Need to consider only the machines that are having update strategy in-place and orchestration type manual.
			if !gardenerutils.IsMachineDeploymentStrategyManualInPlace(machineDeployment.Spec.Strategy) {
				return false
			}

			oldCond := GetMachineCondition(oldMachine, machinev1alpha1.NodeInPlaceUpdate)
			newCond := GetMachineCondition(newMachine, machinev1alpha1.NodeInPlaceUpdate)

			// Consider only the condition transition from UpdateCandidate to SelectedForUpdate
			return oldCond != nil && newCond != nil && oldCond.Reason == machinev1alpha1.CandidateForUpdate && newCond.Reason == machinev1alpha1.SelectedForUpdate
		},
		DeleteFunc: func(_ event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(_ event.GenericEvent) bool {
			return false
		},
	}
}
