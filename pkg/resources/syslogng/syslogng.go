// Copyright © 2022 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package syslogng

import (
	"context"
	"fmt"
	"time"

	"emperror.dev/errors"
	"github.com/banzaicloud/logging-operator/pkg/resources"
	"github.com/banzaicloud/logging-operator/pkg/sdk/logging/api/v1beta1"
	"github.com/banzaicloud/operator-tools/pkg/reconciler"
	"github.com/banzaicloud/operator-tools/pkg/secret"
	"github.com/banzaicloud/operator-tools/pkg/utils"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	SecretConfigName      = "syslogng"
	AppSecretConfigName   = "syslogng-app"
	ConfigCheckKey        = "generated.conf"
	ConfigKey             = "syslogng.conf"
	AppConfigKey          = "syslogng.conf"
	StatefulSetName       = "syslogng"
	PodSecurityPolicyName = "syslogng"
	ServiceName           = "syslogng"
	OutputSecretName      = "syslogng-output"
	OutputSecretPath      = "/syslogng/secret"

	bufferPath                     = "/buffers"
	defaultServiceAccountName      = "syslogng"
	roleBindingName                = "syslogng"
	roleName                       = "syslogng"
	clusterRoleBindingName         = "syslogng"
	clusterRoleName                = "syslogng"
	containerName                  = "syslogng"
	defaultBufferVolumeMetricsPort = 9200
)

// Reconciler holds info what resource to reconcile
type Reconciler struct {
	Logging *v1beta1.Logging
	*reconciler.GenericResourceReconciler
	config  *string
	secrets *secret.MountSecrets
}

type Desire struct {
	DesiredObject runtime.Object
	DesiredState  reconciler.DesiredState
	// BeforeUpdateHook has the ability to change the desired object
	// or even to change the desired state in case the object should be recreated
	BeforeUpdateHook func(runtime.Object) (reconciler.DesiredState, error)
}

func (r *Reconciler) getServiceAccount() string {
	if r.Logging.Spec.SyslogNGSpec.Security.ServiceAccount != "" {
		return r.Logging.Spec.SyslogNGSpec.Security.ServiceAccount
	}
	return r.Logging.QualifiedName(defaultServiceAccountName)
}

func New(client client.Client, log logr.Logger,
	logging *v1beta1.Logging, config *string, secrets *secret.MountSecrets, opts reconciler.ReconcilerOpts) *Reconciler {
	return &Reconciler{
		Logging:                   logging,
		GenericResourceReconciler: reconciler.NewGenericReconciler(client, log, opts),
		config:                    config,
		secrets:                   secrets,
	}
}

// Reconcile reconciles the syslog-ng resource
func (r *Reconciler) Reconcile() (*reconcile.Result, error) {
	ctx := context.Background()
	patchBase := client.MergeFrom(r.Logging.DeepCopy())

	for _, res := range []resources.Resource{
		r.serviceAccount,
		r.role,
		r.roleBinding,
		r.clusterRole,
		r.clusterRoleBinding,
		r.clusterPodSecurityPolicy,
		r.pspRole,
		r.pspRoleBinding,
	} {
		o, state, err := res()
		if err != nil {
			return nil, errors.WrapIf(err, "failed to create desired object")
		}
		if o == nil {
			return nil, errors.Errorf("Reconcile error! Resource %#v returns with nil object", res)
		}
		result, err := r.ReconcileResource(o, state)
		if err != nil {
			return nil, errors.WrapIf(err, "failed to reconcile resource")
		}
		if result != nil {
			return result, nil
		}
	}
	// Config check and cleanup if enabled
	if !r.Logging.Spec.FlowConfigCheckDisabled { //nolint:nestif
		hash, err := r.configHash()
		if err != nil {
			return nil, err
		}
		if result, ok := r.Logging.Status.ConfigCheckResults[hash]; ok {
			// We already have an existing configcheck result:
			// - bail out if it was unsuccessful
			// - cleanup previous results if it's successful
			if !result {
				return nil, errors.Errorf("current config is invalid")
			}
			var removedHashes []string
			if removedHashes, err = r.configCheckCleanup(hash); err != nil {
				r.Log.Error(err, "failed to cleanup resources")
			} else {
				if len(removedHashes) > 0 {
					for _, removedHash := range removedHashes {
						delete(r.Logging.Status.ConfigCheckResults, removedHash)
					}
					if err := r.Client.Status().Patch(ctx, r.Logging, patchBase); err != nil {
						return nil, errors.WrapWithDetails(err, "failed to patch status", "logging", r.Logging)
					} else {
						// explicitly ask for a requeue to short circuit the controller loop after the status update
						return &reconcile.Result{Requeue: true}, nil
					}
				}
			}
		} else {
			// We don't have an existing result
			// - let's create what's necessary to have one
			// - if the result is ready write it into the status
			result, err := r.configCheck(ctx)
			if err != nil {
				return nil, errors.WrapIf(err, "failed to validate config")
			}
			if result.Ready {
				r.Logging.Status.ConfigCheckResults[hash] = result.Valid
				if err := r.Client.Status().Patch(ctx, r.Logging, patchBase); err != nil {
					return nil, errors.WrapWithDetails(err, "failed to patch status", "logging", r.Logging)
				} else {
					// explicitly ask for a requeue to short circuit the controller loop after the status update
					return &reconcile.Result{Requeue: true}, nil
				}
			} else {
				if result.Message != "" {
					r.Log.Info(result.Message)
				} else {
					r.Log.Info("still waiting for the configcheck result...")
				}
				return &reconcile.Result{RequeueAfter: time.Minute}, nil
			}
		}
	}
	// Prepare output secret
	outputSecret, outputSecretDesiredState, err := r.outputSecret(r.secrets, OutputSecretPath)
	if err != nil {
		return nil, errors.WrapIf(err, "failed to create output secret")
	}
	result, err := r.ReconcileResource(outputSecret, outputSecretDesiredState)
	if err != nil {
		return nil, errors.WrapIf(err, "failed to reconcile resource")
	}
	if result != nil {
		return result, nil
	}
	// Mark watched secrets
	secretList, state, err := r.markSecrets(r.secrets)
	if err != nil {
		return nil, errors.WrapIf(err, "failed to mark secrets")
	}
	for _, obj := range secretList {
		result, err := r.ReconcileResource(obj, state)
		if err != nil {
			return nil, errors.WrapIf(err, "failed to reconcile resource")
		}
		if result != nil {
			return result, nil
		}
	}
	for _, res := range []resources.Resource{
		r.secretConfig,
		r.appConfigSecret,
		r.statefulset,
		r.service,
		r.headlessService,
		r.serviceMetrics,
		r.monitorServiceMetrics,
		r.serviceBufferMetrics,
		r.monitorBufferServiceMetrics,
		r.prometheusRules,
		r.bufferVolumePrometheusRules,
	} {
		o, state, err := res()
		if err != nil {
			return nil, errors.WrapIf(err, "failed to create desired object")
		}
		if o == nil {
			return nil, errors.Errorf("Reconcile error! Resource %#v returns with nil object", res)
		}
		result, err := r.ReconcileResource(o, state)
		if err != nil {
			return nil, errors.WrapIf(err, "failed to reconcile resource")
		}
		if result != nil {
			return result, nil
		}
	}

	if res, err := r.reconcileDrain(ctx); res != nil || err != nil {
		return res, err
	}

	return nil, nil
}

func (r *Reconciler) reconcileDrain(ctx context.Context) (*reconcile.Result, error) {
	if r.Logging.Spec.SyslogNGSpec.DisablePvc || !r.Logging.Spec.SyslogNGSpec.Scaling.Drain.Enabled {
		r.Log.Info("syslog-ng buffer draining is disabled")
		return nil, nil
	}

	nsOpt := client.InNamespace(r.Logging.Spec.ControlNamespace)
	syslogNGLabelSet := r.Logging.GetSyslogNGLabels(ComponentSyslogNG)

	var pvcList corev1.PersistentVolumeClaimList
	if err := r.Client.List(ctx, &pvcList, nsOpt,
		client.MatchingLabelsSelector{
			Selector: labels.SelectorFromSet(syslogNGLabelSet).Add(drainableRequirement),
		}); err != nil {
		return nil, errors.WrapIf(err, "listing PVC resources")
	}

	var stsPods corev1.PodList
	if err := r.Client.List(ctx, &stsPods, nsOpt, client.MatchingLabels(syslogNGLabelSet)); err != nil {
		return nil, errors.WrapIf(err, "listing StatefulSet pods")
	}

	bufVolName := r.Logging.QualifiedName(r.Logging.Spec.SyslogNGSpec.BufferStorageVolume.PersistentVolumeClaim.PersistentVolumeSource.ClaimName)

	pvcsInUse := make(map[string]bool)
	for _, pod := range stsPods.Items {
		if bufVol := findVolumeByName(pod.Spec.Volumes, bufVolName); bufVol != nil {
			pvcsInUse[bufVol.PersistentVolumeClaim.ClaimName] = true
		}
	}

	replicaCount, err := NewDataProvider(r.Client).GetReplicaCount(ctx, r.Logging)
	if err != nil {
		return nil, errors.WrapIf(err, "get replica count for syslog-ng")
	}

	// mark PVCs required for upscaling as in-use
	for i := int32(0); i < utils.PointerToInt32(replicaCount); i++ {
		pvcsInUse[fmt.Sprintf("%s-%s-%d", bufVolName, r.Logging.QualifiedName(StatefulSetName), i)] = true
	}

	var jobList batchv1.JobList
	if err := r.Client.List(ctx, &jobList, nsOpt, client.MatchingLabels(r.Logging.GetSyslogNGLabels(ComponentDrainer))); err != nil {
		return nil, errors.WrapIf(err, "listing buffer drainer jobs")
	}

	jobOfPVC := make(map[string]batchv1.Job)
	for _, job := range jobList.Items {
		if bufVol := findVolumeByName(job.Spec.Template.Spec.Volumes, bufVolName); bufVol != nil {
			jobOfPVC[bufVol.PersistentVolumeClaim.ClaimName] = job
		}
	}

	var cr reconciler.CombinedResult
	for _, pvc := range pvcList.Items {
		pvcLog := r.Log.WithValues("pvc", pvc.Name)

		drained := markedAsDrained(pvc)
		inUse := pvcsInUse[pvc.Name]
		if drained && inUse {
			pvcLog.Info("removing drained label from PVC as it has a matching statefulset pod")

			patch := client.MergeFrom(pvc.DeepCopy())
			delete(pvc.Labels, drainStatusLabelKey)
			if err := client.IgnoreNotFound(r.Client.Patch(ctx, pvc.DeepCopy(), patch)); err != nil {
				cr.CombineErr(errors.WrapIf(err, "removing drained label from pvc"))
			}
			continue
		}

		job, hasJob := jobOfPVC[pvc.Name]
		if hasJob && jobSuccessfullyCompleted(job) {
			pvcLog.Info("drainer job for PVC has completed, adding drained label and deleting job")

			patch := client.MergeFrom(pvc.DeepCopy())
			pvc.Labels[drainStatusLabelKey] = drainStatusLabelValue
			if err := client.IgnoreNotFound(r.Client.Patch(ctx, pvc.DeepCopy(), patch)); err != nil {
				cr.CombineErr(errors.WrapIf(err, "marking pvc as drained"))
				continue
			}

			if err := client.IgnoreNotFound(r.Client.Delete(ctx, &job, client.PropagationPolicy(v1.DeletePropagationBackground))); err != nil {
				cr.CombineErr(errors.WrapIf(err, "deleting completed drainer job"))
				continue
			}

			if res, err := r.ReconcileResource(r.placeholderPodFor(pvc), reconciler.StateAbsent); err != nil {
				cr.Combine(res, errors.WrapIfWithDetails(err, "removing placeholder pod for pvc", "pvc", pvc.Name))
				continue
			}
			continue
		}

		if inUse && hasJob {
			pvcLog.Info("deleting drainer job early as PVC is now in use")

			if err := client.IgnoreNotFound(r.Client.Delete(ctx, &job, client.PropagationPolicy(v1.DeletePropagationForeground))); err != nil {
				cr.CombineErr(errors.WrapIf(err, "deleting unnecessary drainer job"))
				continue
			}

			if res, err := r.ReconcileResource(r.placeholderPodFor(pvc), reconciler.StateAbsent); err != nil {
				cr.Combine(res, errors.WrapIfWithDetails(err, "removing placeholder pod for pvc", "pvc", pvc.Name))
				continue
			}
			continue
		}

		if hasJob && !jobSuccessfullyCompleted(job) {
			if job.Status.Failed > 0 {
				cr.CombineErr(errors.NewWithDetails("draining PVC failed", "pvc", pvc.Name, "attempts", job.Status.Failed))
			} else {
				pvcLog.Info("drainer job for PVC has not yet been completed")
			}
			continue
		}

		if !drained && !inUse && !hasJob {
			pvcLog.Info("creating drainer job for PVC")

			if res, err := r.ReconcileResource(r.placeholderPodFor(pvc), reconciler.StatePresent); err != nil {
				cr.Combine(res, errors.WrapIfWithDetails(err, "ensuring placeholder pod is present for pvc", "pvc", pvc.Name))
				continue
			}

			if job, err := r.drainerJobFor(pvc); err != nil {
				cr.CombineErr(errors.WrapIf(err, "assembling drainer job"))
			} else {
				cr.Combine(r.ReconcileResource(job, reconciler.StatePresent))
			}
			continue
		}
	}
	var res *reconcile.Result
	if !cr.Result.IsZero() {
		res = &cr.Result
	}
	return res, cr.Err
}

func RegisterWatches(builder *builder.Builder) *builder.Builder {
	return builder.
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{})
}

var drainableRequirement = requirementMust(labels.NewRequirement("logging.banzaicloud.io/drain", selection.NotEquals, []string{"no"}))

func requirementMust(req *labels.Requirement, err error) labels.Requirement {
	if err != nil {
		panic(err)
	}
	if req == nil {
		panic("requirement is nil")
	}
	return *req
}

const drainStatusLabelKey = "logging.banzaicloud.io/drain-status"
const drainStatusLabelValue = "drained"

func markedAsDrained(pvc corev1.PersistentVolumeClaim) bool {
	return pvc.Labels[drainStatusLabelKey] == drainStatusLabelValue
}

func findVolumeByName(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		vol := &vols[i]
		if vol.Name == name {
			return vol
		}
	}
	return nil
}

func jobSuccessfullyCompleted(job batchv1.Job) bool {
	return job.Status.CompletionTime != nil && job.Status.Succeeded > 0
}
