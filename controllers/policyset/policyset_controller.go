// Copyright (c) 2022 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package controllers

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	appsv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/placementrule/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	policyv1 "github.com/stolostron/governance-policy-propagator/api/v1"
)

const ControllerName string = "policy-set"

var log = ctrl.Log.WithName(ControllerName)

// PolicySetReconciler reconciles a PolicySet object
type PolicySetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// blank assignment to verify that PolicySetReconciler implements reconcile.Reconciler
var _ reconcile.Reconciler = &PolicySetReconciler{}

//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policysets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policysets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policysets/finalizers,verbs=update

func (r *PolicySetReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	log.Info("Reconciling policy sets...")
	// Fetch the PolicySet instance
	instance := &policyv1.PolicySet{}

	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Policy set not found, so it may have been deleted.")

			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to retrieve policy set")

		return reconcile.Result{}, err
	}

	log.V(1).Info("Policy set was found, processing it")

	setNeedsUpdate := processPolicySet(ctx, r.Client, instance)

	if setNeedsUpdate {
		log.Info("Status update needed")

		faultyPlcSet, err := updatePolicySetStatus(ctx, r.Client, instance)
		if err != nil {
			log.Error(err, fmt.Sprintf("reason: policy update error: policy/%v, namespace: %v",
				faultyPlcSet.Name, faultyPlcSet.Namespace))

			return reconcile.Result{}, err
		}
	}

	log.Info("Policy set successfully processed, reconcile complete.")

	r.Recorder.Event(
		instance,
		"Normal",
		fmt.Sprintf("policySet: %s", instance.GetName()),
		fmt.Sprintf("Status successfully updated for policySet %s in namespace %s", instance.GetName(),
			instance.GetNamespace()),
	)

	return reconcile.Result{}, nil
}

// processPolicySet compares the status of a policyset to its desired state and determines whether an update is needed
func processPolicySet(ctx context.Context, c client.Client, plcSet *policyv1.PolicySet) bool {
	log.V(1).Info("Processing policy sets")

	needsUpdate := false

	// compile results and compliance state from policy statuses
	generatedResults := []policyv1.PolicySetStatusResult{}
	complianceFound := false
	aggregatedCompliance := "Compliant"
	placementsByBinding := map[string]policyv1.PolicySetStatusPlacement{}

	for i := range plcSet.Spec.Policies {
		childPlcName := plcSet.Spec.Policies[i]
		childNamespacedName := types.NamespacedName{
			Name:      string(childPlcName),
			Namespace: plcSet.Namespace,
		}

		childPlc := &policyv1.Policy{}

		err := c.Get(ctx, childNamespacedName, childPlc)
		if err != nil {
			// policy does not exist, return error message
			var errMessage string
			if errors.IsNotFound(err) {
				errMessage = string(childPlcName) + " not found"
			} else {
				errMessage = strings.Split(err.Error(), "Policy.policy.open-cluster-management.io ")[1]
			}

			generatedResults = append(generatedResults, policyv1.PolicySetStatusResult{
				Policy:  string(childPlcName),
				Message: errMessage,
			})
		} else {
			// policy exists - can use it to calculate status data
			log.V(1).Info("Evaluating changes in policy " + string(childPlcName))
			if childPlc.Spec.Disabled {
				generatedResults = append(generatedResults, policyv1.PolicySetStatusResult{
					Policy:  string(childPlcName),
					Message: string(childPlcName) + " is disabled",
				})
			} else {
				generatedResults = append(generatedResults, policyv1.PolicySetStatusResult{
					Policy:    string(childPlcName),
					Compliant: string(childPlc.Status.ComplianceState),
					Clusters:  statusToClusters(childPlc.Status.Status),
				})
			}

			// aggregate compliance state
			if string(childPlc.Status.ComplianceState) != "" {
				complianceFound = true
			}
			if string(childPlc.Status.ComplianceState) == "NonCompliant" {
				aggregatedCompliance = "NonCompliant"
			}

			// aggregate placements
			for _, placement := range childPlc.Status.Placement {
				if placement.PolicySet == plcSet.GetName() {
					placementsByBinding[placement.PlacementBinding] = plcPlacementToSetPlacement(*placement)
				}
			}
		}
	}

	generatedPlacements := []policyv1.PolicySetStatusPlacement{}
	for _, pcmt := range placementsByBinding {
		generatedPlacements = append(generatedPlacements, pcmt)
	}

	builtStatus := policyv1.PolicySetStatus{
		Results:   generatedResults,
		Placement: generatedPlacements,
	}

	if complianceFound {
		builtStatus.Compliant = aggregatedCompliance
	}

	if !equality.Semantic.DeepEqual(plcSet.Status, builtStatus) {
		plcSet.Status = *builtStatus.DeepCopy()
		needsUpdate = true
	}

	return needsUpdate
}

// updatePolicySetStatus triggers an update on the status of a policy set that needs it
func updatePolicySetStatus(ctx context.Context, c client.Client, policySet *policyv1.PolicySet) (*policyv1.PolicySet,
	error) {
	err := c.Status().Update(ctx, policySet)
	if err != nil {
		return policySet, err
	}

	return nil, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PolicySetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(
			&policyv1.PolicySet{},
			builder.WithPredicates(policySetPredicateFuncs)).
		Watches(
			&source.Kind{Type: &policyv1.Policy{}},
			handler.EnqueueRequestsFromMapFunc(policyMapper(mgr.GetClient())),
			builder.WithPredicates(policyPredicateFuncs)).
		Watches(
			&source.Kind{Type: &policyv1.PlacementBinding{}},
			handler.EnqueueRequestsFromMapFunc(placementBindingMapper(mgr.GetClient())),
			builder.WithPredicates(pbPredicateFuncs)).
		Watches(
			&source.Kind{Type: &appsv1.PlacementRule{}},
			handler.EnqueueRequestsFromMapFunc(placementRuleMapper(mgr.GetClient()))).
		Watches(
			&source.Kind{Type: &clusterv1alpha1.PlacementDecision{}},
			handler.EnqueueRequestsFromMapFunc(placementDecisionMapper(mgr.GetClient()))).
		Complete(r)
}

// Helper function to convert policy.status.status to policyset.status.results.clusters
func statusToClusters(status []*policyv1.CompliancePerClusterStatus) []policyv1.PolicySetResultCluster {
	clusters := []policyv1.PolicySetResultCluster{}

	for i := range status {
		clusters = append(clusters, policyv1.PolicySetResultCluster{
			ClusterName:      status[i].ClusterName,
			ClusterNamespace: status[i].ClusterNamespace,
			Compliant:        string(status[i].ComplianceState),
		})
	}

	return clusters
}

// Helper function to convert policy placement to policyset placement
func plcPlacementToSetPlacement(plcPlacement policyv1.Placement) policyv1.PolicySetStatusPlacement {
	return policyv1.PolicySetStatusPlacement{
		PlacementBinding: plcPlacement.PlacementBinding,
		Placement:        plcPlacement.Placement,
		PlacementRule:    plcPlacement.PlacementRule,
	}
}