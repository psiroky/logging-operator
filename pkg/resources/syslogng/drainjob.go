// Copyright © 2021 Banzai Cloud
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
	"strings"

	"github.com/banzaicloud/logging-operator/pkg/sdk/logging/api/v1beta1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *Reconciler) drainerJobFor(pvc corev1.PersistentVolumeClaim) (*batchv1.Job, error) {
	bufVolName := r.Logging.QualifiedName(r.Logging.Spec.SyslogNGSpec.BufferStorageVolume.PersistentVolumeClaim.PersistentVolumeSource.ClaimName)

	syslogNGContainer := syslogNGContainer(withoutSyslogNGOutLogrotate(r.Logging.Spec.SyslogNGSpec))
	syslogNGContainer.VolumeMounts = append(syslogNGContainer.VolumeMounts, corev1.VolumeMount{
		Name:      bufVolName,
		MountPath: bufferPath,
	})
	containers := []corev1.Container{
		syslogNGContainer,
		drainWatchContainer(&r.Logging.Spec.SyslogNGSpec.Scaling.Drain, bufVolName),
	}
	if c := r.bufferMetricsSidecarContainer(); c != nil {
		containers = append(containers, *c)
	}

	spec := batchv1.JobSpec{
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      r.Logging.GetSyslogNGLabels(ComponentDrainer),
				Annotations: r.Logging.Spec.SyslogNGSpec.Scaling.Drain.Annotations,
			},
			Spec: corev1.PodSpec{
				Volumes:                   r.generateVolume(),
				ServiceAccountName:        r.getServiceAccount(),
				ImagePullSecrets:          r.Logging.Spec.SyslogNGSpec.Image.ImagePullSecrets,
				Containers:                containers,
				NodeSelector:              r.Logging.Spec.SyslogNGSpec.NodeSelector,
				Tolerations:               r.Logging.Spec.SyslogNGSpec.Tolerations,
				Affinity:                  r.Logging.Spec.SyslogNGSpec.Affinity,
				TopologySpreadConstraints: r.Logging.Spec.SyslogNGSpec.TopologySpreadConstraints,
				PriorityClassName:         r.Logging.Spec.SyslogNGSpec.PodPriorityClassName,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: r.Logging.Spec.SyslogNGSpec.Security.PodSecurityContext.RunAsNonRoot,
					FSGroup:      r.Logging.Spec.SyslogNGSpec.Security.PodSecurityContext.FSGroup,
					RunAsUser:    r.Logging.Spec.SyslogNGSpec.Security.PodSecurityContext.RunAsUser,
					RunAsGroup:   r.Logging.Spec.SyslogNGSpec.Security.PodSecurityContext.RunAsGroup,
				},
				RestartPolicy: corev1.RestartPolicyNever,
			},
		},
	}

	spec.Template.Spec.Volumes = append(spec.Template.Spec.Volumes, corev1.Volume{
		Name: bufVolName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
			},
		},
	})
	for _, n := range r.Logging.Spec.SyslogNGSpec.ExtraVolumes {
		if err := n.ApplyVolumeForPodSpec(&spec.Template.Spec); err != nil {
			return nil, err
		}
	}
	return &batchv1.Job{
		ObjectMeta: r.SyslogNGObjectMeta(StatefulSetName+pvc.Name[strings.LastIndex(pvc.Name, "-"):]+"-drainer", ComponentDrainer),
		Spec:       spec,
	}, nil
}

func drainWatchContainer(cfg *v1beta1.SyslogNGDrainConfig, bufferVolumeName string) corev1.Container {
	return corev1.Container{
		Env: []corev1.EnvVar{
			{
				Name:  "BUFFER_PATH",
				Value: bufferPath,
			},
		},
		Image:           cfg.Image.RepositoryWithTag(),
		ImagePullPolicy: corev1.PullPolicy(cfg.Image.PullPolicy),
		Name:            "drain-watch",
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: bufferPath,
				Name:      bufferVolumeName,
				ReadOnly:  true,
			},
		},
	}
}

func withoutSyslogNGOutLogrotate(spec *v1beta1.SyslogNGSpec) *v1beta1.SyslogNGSpec {
	res := spec.DeepCopy()
	res.SyslogNGOutLogrotate = nil
	return res
}
