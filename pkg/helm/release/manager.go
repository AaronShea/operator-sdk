// Copyright 2018 The Operator-SDK Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	cpb "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/kube"
	rpb "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"

	"github.com/mattbaird/jsonpatch"
	"github.com/operator-framework/operator-sdk/pkg/helm/internal/types"
)

// Manager manages a Helm release. It can install, update, reconcile,
// and uninstall a release.
type Manager interface {
	ReleaseName() string
	IsInstalled() bool
	IsUpdateRequired() bool
	Sync(context.Context) error
	InstallRelease(context.Context) (*rpb.Release, error)
	UpdateRelease(context.Context) (*rpb.Release, *rpb.Release, error)
	ReconcileRelease(context.Context) (*rpb.Release, error)
	UninstallRelease(context.Context) (*rpb.Release, error)
}

type manager struct {
	actionConfig   *action.Configuration
	storageBackend *storage.Storage
	kubeClient     kube.Interface

	releaseName string
	namespace   string

	values map[string]interface{}
	status *types.HelmAppStatus

	isInstalled      bool
	isUpdateRequired bool
	deployedRelease  *rpb.Release
	chart            *cpb.Chart
}

// ReleaseName returns the name of the release.
func (m manager) ReleaseName() string {
	return m.releaseName
}

func (m manager) IsInstalled() bool {
	return m.isInstalled
}

func (m manager) IsUpdateRequired() bool {
	return m.isUpdateRequired
}

// Sync ensures the Helm storage backend is in sync with the status of the
// custom resource.
func (m *manager) Sync(ctx context.Context) error {
	// Get release history for this release name
	releases, err := m.storageBackend.History(m.releaseName)
	if err != nil && !notFoundErr(err) {
		return fmt.Errorf("failed to retrieve release history: %w", err)
	}

	// Cleanup non-deployed release versions. If all release versions are
	// non-deployed, this will ensure that failed installations are correctly
	// retried.
	for _, rel := range releases {
		if rel.Info != nil && rel.Info.Status != rpb.StatusDeployed {
			_, err := m.storageBackend.Delete(rel.Name, rel.Version)
			if err != nil && !notFoundErr(err) {
				return fmt.Errorf("failed to delete stale release version: %w", err)
			}
		}
	}

	// Load the most recently deployed release from the storage backend.
	deployedRelease, err := m.getDeployedRelease()
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get deployed release: %w", err)
	}
	m.deployedRelease = deployedRelease
	m.isInstalled = true

	// Get the next candidate release to determine if an update is necessary.
	candidateRelease, err := m.getCandidateRelease(m.namespace, m.releaseName, m.chart, m.values)
	if err != nil {
		return fmt.Errorf("failed to get candidate release: %w", err)
	}
	if deployedRelease.Manifest != candidateRelease.Manifest {
		m.isUpdateRequired = true
	}

	return nil
}

func notFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func (m manager) getDeployedRelease() (*rpb.Release, error) {
	deployedRelease, err := m.storageBackend.Deployed(m.releaseName)
	if err != nil {
		if strings.Contains(err.Error(), "has no deployed releases") {
			return nil, driver.ErrReleaseNotFound
		}
		return nil, err
	}
	return deployedRelease, nil
}

func (m manager) getCandidateRelease(namespace, name string, chart *cpb.Chart,
	values map[string]interface{}) (*rpb.Release, error) {
	upgrade := action.NewUpgrade(m.actionConfig)
	upgrade.Namespace = namespace
	upgrade.DryRun = true
	return upgrade.Run(name, chart, values)
}

// InstallRelease performs a Helm release install.
func (m manager) InstallRelease(ctx context.Context) (*rpb.Release, error) {
	install := action.NewInstall(m.actionConfig)
	install.ReleaseName = m.releaseName
	install.Namespace = m.namespace

	installedRelease, err := install.Run(m.chart, m.values)
	if err != nil {
		// Workaround for helm/helm#3338
		if installedRelease != nil {
			uninstall := action.NewUninstall(m.actionConfig)
			_, uninstallErr := uninstall.Run(m.releaseName)

			// In certain cases, InstallRelease will return a partial release in
			// the response even when it doesn't record the release in its release
			// store (e.g. when there is an error rendering the release manifest).
			// In that case the rollback will fail with a not found error because
			// there was nothing to rollback.
			//
			// Only log a message about a rollback failure if the failure was caused
			// by something other than the release not being found.
			if uninstallErr != nil && !notFoundErr(uninstallErr) {
				return nil, fmt.Errorf("failed installation (%s) and failed rollback: %w", err, uninstallErr)
			}
		}
		return nil, fmt.Errorf("failed to install release: %w", err)
	}
	return installedRelease, nil
}

// UpdateRelease performs a Helm release update.
func (m manager) UpdateRelease(ctx context.Context) (*rpb.Release, *rpb.Release, error) {
	upgrade := action.NewUpgrade(m.actionConfig)
	upgrade.Namespace = m.namespace

	updatedRelease, err := upgrade.Run(m.releaseName, m.chart, m.values)
	if err != nil {
		// Workaround for helm/helm#3338
		if updatedRelease != nil {
			rollback := action.NewRollback(m.actionConfig)
			rollback.Force = true

			// As of Helm 2.13, if UpdateRelease returns a non-nil release, that
			// means the release was also recorded in the release store.
			// Therefore, we should perform the rollback when we have a non-nil
			// release. Any rollback error here would be unexpected, so always
			// log both the update and rollback errors.
			rollbackErr := rollback.Run(m.releaseName)
			if rollbackErr != nil {
				return nil, nil, fmt.Errorf("failed update (%s) and failed rollback: %w", err, rollbackErr)
			}
		}
		return nil, nil, fmt.Errorf("failed to update release: %w", err)
	}
	return m.deployedRelease, updatedRelease, err
}

// ReconcileRelease creates or patches resources as necessary to match the
// deployed release's manifest.
func (m manager) ReconcileRelease(ctx context.Context) (*rpb.Release, error) {
	err := reconcileRelease(ctx, m.kubeClient, m.deployedRelease.Manifest)
	return m.deployedRelease, err
}

func reconcileRelease(ctx context.Context, kubeClient kube.Interface, expectedManifest string) error {
	expectedInfos, err := kubeClient.Build(bytes.NewBufferString(expectedManifest), false)
	if err != nil {
		return err
	}
	return expectedInfos.Visit(func(expected *resource.Info, err error) error {
		if err != nil {
			return err
		}

		expectedClient := resource.NewClientWithOptions(expected.Client, func(r *rest.Request) {
			*r = *r.Context(ctx)
		})
		helper := resource.NewHelper(expectedClient, expected.Mapping)

		existing, err := helper.Get(expected.Namespace, expected.Name, false)
		if apierrors.IsNotFound(err) {
			if _, err := helper.Create(expected.Namespace, true, expected.Object,
				&metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create error: %s", err)
			}
			return nil
		} else if err != nil {
			return err
		}

		patch, err := generatePatch(existing, expected.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal JSON patch: %w", err)
		}

		if patch == nil {
			return nil
		}

		_, err = helper.Patch(expected.Namespace, expected.Name, apitypes.JSONPatchType, patch, &metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("patch error: %w", err)
		}
		return nil
	})
}

func generatePatch(existing, expected runtime.Object) ([]byte, error) {
	existingJSON, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}

	ops, err := jsonpatch.CreatePatch(existingJSON, expectedJSON)
	if err != nil {
		return nil, err
	}

	// We ignore the "remove" operations from the full patch because they are
	// fields added by Kubernetes or by the user after the existing release
	// resource has been applied. The goal for this patch is to make sure that
	// the fields managed by the Helm chart are applied.
	patchOps := make([]jsonpatch.JsonPatchOperation, 0)
	for _, op := range ops {
		if op.Operation != "remove" {
			patchOps = append(patchOps, op)
		}
	}

	// If there are no patch operations, return nil. Callers are expected
	// to check for a nil response and skip the patch operation to avoid
	// unnecessary chatter with the API server.
	if len(patchOps) == 0 {
		return nil, nil
	}

	return json.Marshal(patchOps)
}

// UninstallRelease performs a Helm release uninstall.
func (m manager) UninstallRelease(ctx context.Context) (*rpb.Release, error) {
	// Get history of this release
	h, err := m.storageBackend.History(m.releaseName)
	if err != nil {
		return nil, fmt.Errorf("failed to get release history: %w", err)
	}

	// If there is no history, the release has already been uninstalled,
	// so return ErrReleaseNotFound.
	if len(h) == 0 {
		return nil, driver.ErrReleaseNotFound
	}

	uninstall := action.NewUninstall(m.actionConfig)
	uninstallResponse, err := uninstall.Run(m.releaseName)
	return uninstallResponse.Release, err
}
