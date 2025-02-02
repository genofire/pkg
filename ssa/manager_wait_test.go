/*
Copyright 2021 Stefan Prodan
Copyright 2021 The Flux authors

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

package ssa

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/cli-utils/pkg/object"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWaitForSet(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	id := generateName("wait")
	objects, err := readManifest("testdata/test5.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "infra", "default")

	_, crd := getFirstObject(objects, "CustomResourceDefinition", "clustertests.testing.fluxcd.io")
	_, cr := getFirstObject(objects, "ClusterTest", id)

	t.Run("waits for CRD and CR", func(t *testing.T) {
		cs, err := manager.Apply(ctx, crd, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		if err := manager.WaitForSet([]object.ObjMetadata{cs.ObjMetadata}, DefaultWaitOptions()); err != nil {
			t.Errorf("wait failed for CRD: %v", err)
		}

		changeSet, err := manager.ApplyAll(ctx, objects, DefaultApplyOptions())
		if err != nil {
			t.Fatal(err)
		}

		if err := manager.WaitForSet(changeSet.ToObjMetadataSet(), WaitOptions{time.Second, 3 * time.Second, false}); err == nil {
			t.Error("wanted wait error due to observedGeneration < generation")
		}

		clusterCR := &unstructured.Unstructured{}
		clusterCR.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "testing.fluxcd.io",
			Kind:    "ClusterTest",
			Version: "v1",
		})
		if err := manager.client.Get(ctx, client.ObjectKeyFromObject(cr), clusterCR); err != nil {
			t.Fatal(err)
		}

		var observedGeneration int64 = 1
		clusterCR.SetManagedFields(nil)
		err = unstructured.SetNestedField(clusterCR.Object, observedGeneration, "status", "observedGeneration")
		if err != nil {
			t.Fatal(err)
		}

		opts := &client.SubResourcePatchOptions{
			PatchOptions: client.PatchOptions{
				FieldManager: manager.owner.Field,
			},
		}

		if err := manager.client.Status().Patch(ctx, clusterCR, client.Apply, opts); err != nil {
			t.Fatal(err)
		}

		if err := manager.WaitForSet(changeSet.ToObjMetadataSet(), DefaultWaitOptions()); err != nil {
			t.Errorf("wait error: %v", err)
		}
	})
}

func TestWaitForSet_failFast(t *testing.T) {
	timeout := 5 * time.Second
	interval := 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	id := generateName("failfast")
	objects, err := readManifest("testdata/test10.yaml", id)
	if err != nil {
		t.Fatal(err)
	}

	manager.SetOwnerLabels(objects, "infra", "default")
	_, pvc := getFirstObject(objects, "PersistentVolumeClaim", id)
	_, deploy := getFirstObject(objects, "Deployment", id)

	deployObjMeta, _ := object.RuntimeToObjMeta(deploy)

	cs, err := manager.ApplyAllStaged(ctx, objects, DefaultApplyOptions())
	if err != nil {
		t.Fatal(err)
	}

	var clusterDeploy = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id,
			Namespace: id,
		},
	}
	err = manager.client.Get(ctx, client.ObjectKeyFromObject(deploy), clusterDeploy)
	if err != nil {
		t.Fatal(err)
	}

	// Set Progressing Condition to false and reason to ProgressDeadlineExceeded
	// This tells kstatus that the deployment has stalled.
	cond := appsv1.DeploymentCondition{
		Type:               appsv1.DeploymentProgressing,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.Time{},
		Reason:             "ProgressDeadlineExceeded",
		Message:            "timeout progressing",
	}
	clusterDeploy.Status = appsv1.DeploymentStatus{
		ObservedGeneration:  clusterDeploy.Generation,
		Replicas:            *clusterDeploy.Spec.Replicas,
		UpdatedReplicas:     *clusterDeploy.Spec.Replicas,
		UnavailableReplicas: *clusterDeploy.Spec.Replicas,
		Conditions:          []appsv1.DeploymentCondition{cond},
	}
	err = manager.client.Status().Update(ctx, clusterDeploy)
	if err != nil {
		t.Fatal(err)
	}

	clusterPvc := &unstructured.Unstructured{}
	clusterPvc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Kind:    "PersistentVolumeClaim",
		Version: "v1",
	})
	if err := manager.client.Get(ctx, client.ObjectKeyFromObject(pvc), clusterPvc); err != nil {
		t.Fatal(err)
	}

	if err := unstructured.SetNestedField(clusterPvc.Object, "Bound", "status", "phase"); err != nil {
		t.Fatal(err)
	}

	opts := &client.SubResourcePatchOptions{
		PatchOptions: client.PatchOptions{
			FieldManager: manager.owner.Field,
		},
	}

	clusterPvc.SetManagedFields(nil)
	if err := manager.client.Status().Patch(ctx, clusterPvc, client.Apply, opts); err != nil {
		t.Fatal(err)
	}

	t.Run("timeout when failfast is set to false", func(t *testing.T) {
		err = manager.WaitForSet(cs.ToObjMetadataSet(), WaitOptions{
			Interval: interval,
			Timeout:  timeout,
			FailFast: false,
		})

		deployFailedMsg := fmt.Sprintf("%s status: '%s'", FmtObjMetadata(deployObjMeta), status.FailedStatus)

		if err == nil || !strings.Contains(err.Error(), "timeout waiting for") {
			t.Fatal("expected WaitForSet to timeout waiting for deployment")
		}

		if !strings.Contains(err.Error(), deployFailedMsg) {
			t.Fatal("expected error to contain status of failed deployment")
		}
	})

	t.Run("return early when failfast is set to true", func(t *testing.T) {
		err = manager.WaitForSet(cs.ToObjMetadataSet(), WaitOptions{
			Interval: interval,
			Timeout:  timeout,
			FailFast: true,
		})

		deployFailedMsg := fmt.Sprintf("%s status: '%s'", FmtObjMetadata(deployObjMeta), status.FailedStatus)

		if err == nil || !strings.Contains(err.Error(), "failed early") {
			t.Fatal("expected WaitForSet to fail early due to stalled deployment")
		}

		if !strings.Contains(err.Error(), deployFailedMsg) {
			t.Fatal("expected error to contain status of failed deployment")
		}
	})

	t.Run("fail early even if there are still Progressing resources", func(t *testing.T) {
		// change status to Pending to have an 'InProgress' resource
		clusterPvc := &unstructured.Unstructured{}
		clusterPvc.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "",
			Kind:    "PersistentVolumeClaim",
			Version: "v1",
		})
		if err := manager.client.Get(ctx, client.ObjectKeyFromObject(pvc), clusterPvc); err != nil {
			t.Fatal(err)
		}

		if err := unstructured.SetNestedField(clusterPvc.Object, "Pending", "status", "phase"); err != nil {
			t.Fatal(err)
		}
		opts := &client.SubResourcePatchOptions{
			PatchOptions: client.PatchOptions{
				FieldManager: manager.owner.Field,
			},
		}

		clusterPvc.SetManagedFields(nil)
		if err := manager.client.Status().Patch(ctx, clusterPvc, client.Apply, opts); err != nil {
			t.Fatal(err)
		}

		err = manager.WaitForSet(cs.ToObjMetadataSet(), WaitOptions{
			Interval: interval,
			Timeout:  timeout,
			FailFast: true,
		})

		deployFailedMsg := fmt.Sprintf("%s status: '%s'", FmtObjMetadata(deployObjMeta), status.FailedStatus)

		if err == nil || !strings.Contains(err.Error(), "failed early") {
			t.Fatal("expected WaitForSet to fail early due to stalled deployment")
		}

		if !strings.Contains(err.Error(), deployFailedMsg) {
			t.Fatal("expected error to contain status of failed deployment")
		}
	})
}
