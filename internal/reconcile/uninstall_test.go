/*
Copyright 2022 The Flux authors

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

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	helmrelease "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	helmstorage "helm.sh/helm/v3/pkg/storage"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/fluxcd/pkg/apis/meta"

	helmv2 "github.com/fluxcd/helm-controller/api/v2beta2"
	"github.com/fluxcd/helm-controller/internal/action"
	"github.com/fluxcd/helm-controller/internal/release"
	"github.com/fluxcd/helm-controller/internal/storage"
	"github.com/fluxcd/helm-controller/internal/testutil"
)

func TestUninstall_Reconcile(t *testing.T) {
	mockUpdateErr := errors.New("mock update error")

	tests := []struct {
		name string
		// driver allows for modifying the Helm storage driver.
		driver func(helmdriver.Driver) helmdriver.Driver
		// releases is the list of releases that are stored in the driver
		// before uninstall.
		releases func(namespace string) []*helmrelease.Release
		// spec modifies the HelmRelease Object spec before uninstall.
		spec func(spec *helmv2.HelmReleaseSpec)
		// status to configure on the HelmRelease Object before uninstall.
		status func(releases []*helmrelease.Release) helmv2.HelmReleaseStatus
		// wantErr is the error that is expected to be returned.
		wantErr error
		// expectedConditions are the conditions that are expected to be set on
		// the HelmRelease after running rollback.
		expectConditions []metav1.Condition
		// expectCurrent is the expected Current release information in the
		// HelmRelease after uninstall.
		expectCurrent func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo
		// expectPrevious returns the expected Previous release information of
		// the HelmRelease after uninstall.
		expectPrevious func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo
		// expectFailures is the expected Failures count of the HelmRelease.
		expectFailures int64
		// expectInstallFailures is the expected InstallFailures count of the
		// HelmRelease.
		expectInstallFailures int64
		// expectUpgradeFailures is the expected UpgradeFailures count of the
		// HelmRelease.
		expectUpgradeFailures int64
	}{
		{
			name: "uninstall success",
			releases: func(namespace string) []*helmrelease.Release {
				return []*helmrelease.Release{
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   1,
						Chart:     testutil.BuildChart(testutil.ChartWithTestHook()),
						Status:    helmrelease.StatusDeployed,
					}),
				}
			},
			spec: func(spec *helmv2.HelmReleaseSpec) {
				spec.Uninstall = &helmv2.Uninstall{
					KeepHistory: true,
				}
			},
			status: func(releases []*helmrelease.Release) helmv2.HelmReleaseStatus {
				return helmv2.HelmReleaseStatus{
					Current: release.ObservedToInfo(release.ObserveRelease(releases[0])),
				}
			},
			expectConditions: []metav1.Condition{
				*conditions.FalseCondition(meta.ReadyCondition, helmv2.UninstallSucceededReason,
					"Uninstall of release"),
				*conditions.FalseCondition(helmv2.ReleasedCondition, helmv2.UninstallSucceededReason,
					"Uninstall of release"),
			},
			expectCurrent: func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo {
				return release.ObservedToInfo(release.ObserveRelease(releases[0]))
			},
		},
		{
			name: "uninstall failure",
			releases: func(namespace string) []*helmrelease.Release {
				return []*helmrelease.Release{
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   1,
						Status:    helmrelease.StatusDeployed,
						Chart:     testutil.BuildChart(testutil.ChartWithFailingHook()),
					}, testutil.ReleaseWithFailingHook()),
				}
			},
			spec: func(spec *helmv2.HelmReleaseSpec) {
				spec.Uninstall = &helmv2.Uninstall{
					KeepHistory: true,
				}
			},
			status: func(releases []*helmrelease.Release) helmv2.HelmReleaseStatus {
				return helmv2.HelmReleaseStatus{
					Current: release.ObservedToInfo(release.ObserveRelease(releases[0])),
				}
			},
			expectConditions: []metav1.Condition{
				*conditions.FalseCondition(meta.ReadyCondition, helmv2.UninstallFailedReason,
					"uninstallation completed with 1 error(s): timed out waiting for the condition"),
				*conditions.FalseCondition(helmv2.ReleasedCondition, helmv2.UninstallFailedReason,
					"uninstallation completed with 1 error(s): timed out waiting for the condition"),
			},
			expectCurrent: func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo {
				return release.ObservedToInfo(release.ObserveRelease(releases[0]))
			},
			expectFailures: 1,
		},
		{
			name: "uninstall failure without storage update",
			driver: func(driver helmdriver.Driver) helmdriver.Driver {
				return &storage.Failing{
					// Explicitly inherit the driver, as we want to rely on the
					// Secret storage, as the memory storage does not detach
					// objects from the release action. Causing writes post-persist
					// to leak to the stored release object.
					// xref: https://github.com/helm/helm/issues/11304
					Driver:    driver,
					UpdateErr: mockUpdateErr,
				}
			},
			releases: func(namespace string) []*helmrelease.Release {
				return []*helmrelease.Release{
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   1,
						Chart:     testutil.BuildChart(testutil.ChartWithTestHook()),
						Status:    helmrelease.StatusDeployed,
					}),
				}
			},
			spec: func(spec *helmv2.HelmReleaseSpec) {
				spec.Uninstall = &helmv2.Uninstall{
					KeepHistory: true,
				}
			},
			status: func(releases []*helmrelease.Release) helmv2.HelmReleaseStatus {
				return helmv2.HelmReleaseStatus{
					Current: release.ObservedToInfo(release.ObserveRelease(releases[0])),
				}
			},
			expectConditions: []metav1.Condition{
				*conditions.FalseCondition(meta.ReadyCondition, helmv2.UninstallFailedReason,
					ErrNoStorageUpdate.Error()),
				*conditions.FalseCondition(helmv2.ReleasedCondition, helmv2.UninstallFailedReason,
					ErrNoStorageUpdate.Error()),
			},
			expectCurrent: func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo {
				return release.ObservedToInfo(release.ObserveRelease(releases[0]))
			},
			expectFailures: 1,
			wantErr:        ErrNoStorageUpdate,
		},
		{
			name: "uninstall failure without storage delete",
			driver: func(driver helmdriver.Driver) helmdriver.Driver {
				return &storage.Failing{
					// Explicitly inherit the driver, as we want to rely on the
					// Secret storage, as the memory storage does not detach
					// objects from the release action. Causing writes post-persist
					// to leak to the stored release object.
					// xref: https://github.com/helm/helm/issues/11304
					Driver:    driver,
					DeleteErr: fmt.Errorf("delete error"),
				}
			},
			releases: func(namespace string) []*helmrelease.Release {
				return []*helmrelease.Release{
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   1,
						Status:    helmrelease.StatusDeployed,
						Chart:     testutil.BuildChart(),
					}),
				}
			},
			status: func(releases []*helmrelease.Release) helmv2.HelmReleaseStatus {
				return helmv2.HelmReleaseStatus{
					Current: release.ObservedToInfo(release.ObserveRelease(releases[0])),
				}
			},
			expectConditions: []metav1.Condition{
				*conditions.FalseCondition(meta.ReadyCondition, helmv2.UninstallFailedReason,
					"delete error"),
				*conditions.FalseCondition(helmv2.ReleasedCondition, helmv2.UninstallFailedReason,
					"delete error"),
			},
			expectCurrent: func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo {
				return release.ObservedToInfo(release.ObserveRelease(releases[0]))
			},
			expectFailures: 1,
		},
		{
			name: "uninstall without current",
			releases: func(namespace string) []*helmrelease.Release {
				return []*helmrelease.Release{
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   1,
						Chart:     testutil.BuildChart(),
						Status:    helmrelease.StatusDeployed,
					}),
				}
			},
			expectConditions: []metav1.Condition{},
			wantErr:          ErrNoCurrent,
		},
		{
			name: "uninstall with stale current",
			releases: func(namespace string) []*helmrelease.Release {
				return []*helmrelease.Release{
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   1,
						Chart:     testutil.BuildChart(testutil.ChartWithTestHook()),
						Status:    helmrelease.StatusSuperseded,
					}, testutil.ReleaseWithTestHook()),
					testutil.BuildRelease(&helmrelease.MockReleaseOptions{
						Name:      mockReleaseName,
						Namespace: namespace,
						Version:   2,
						Chart:     testutil.BuildChart(),
						Status:    helmrelease.StatusDeployed,
					}),
				}
			},
			spec: func(spec *helmv2.HelmReleaseSpec) {
				spec.Uninstall = &helmv2.Uninstall{
					KeepHistory: true,
				}
			},
			status: func(releases []*helmrelease.Release) helmv2.HelmReleaseStatus {
				return helmv2.HelmReleaseStatus{
					Current: release.ObservedToInfo(release.ObserveRelease(releases[0])),
				}
			},
			expectConditions: []metav1.Condition{
				*conditions.FalseCondition(meta.ReadyCondition, helmv2.UninstallFailedReason,
					ErrReleaseMismatch.Error()),
				*conditions.FalseCondition(helmv2.ReleasedCondition, helmv2.UninstallFailedReason,
					ErrReleaseMismatch.Error()),
			},
			expectCurrent: func(releases []*helmrelease.Release) *helmv2.HelmReleaseInfo {
				return release.ObservedToInfo(release.ObserveRelease(releases[0]))
			},
			expectFailures: 1,
			wantErr:        ErrReleaseMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			namedNS, err := testEnv.CreateNamespace(context.TODO(), mockReleaseNamespace)
			g.Expect(err).NotTo(HaveOccurred())
			t.Cleanup(func() {
				_ = testEnv.Delete(context.TODO(), namedNS)
			})
			releaseNamespace := namedNS.Name

			var releases []*helmrelease.Release
			if tt.releases != nil {
				releases = tt.releases(releaseNamespace)
				releaseutil.SortByRevision(releases)
			}

			obj := &helmv2.HelmRelease{
				Spec: helmv2.HelmReleaseSpec{
					ReleaseName:      mockReleaseName,
					TargetNamespace:  releaseNamespace,
					StorageNamespace: releaseNamespace,
					Timeout:          &metav1.Duration{Duration: 100 * time.Millisecond},
				},
			}
			if tt.spec != nil {
				tt.spec(&obj.Spec)
			}
			if tt.status != nil {
				obj.Status = tt.status(releases)
			}

			getter, err := RESTClientGetterFromManager(testEnv.Manager, obj.GetReleaseNamespace())
			g.Expect(err).ToNot(HaveOccurred())

			cfg, err := action.NewConfigFactory(getter,
				action.WithStorage(action.DefaultStorageDriver, obj.GetStorageNamespace()),
				action.WithDebugLog(logr.Discard()),
			)
			g.Expect(err).ToNot(HaveOccurred())

			store := helmstorage.Init(cfg.Driver)
			for _, r := range releases {
				g.Expect(store.Create(r)).To(Succeed())
			}

			if tt.driver != nil {
				cfg.Driver = tt.driver(cfg.Driver)
			}

			recorder := record.NewFakeRecorder(10)
			got := NewUninstall(cfg, recorder).Reconcile(context.TODO(), &Request{
				Object: obj,
			})
			if tt.wantErr != nil {
				g.Expect(errors.Is(got, tt.wantErr)).To(BeTrue())
			} else {
				g.Expect(got).ToNot(HaveOccurred())
			}

			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.expectConditions))

			releases, _ = store.History(mockReleaseName)
			releaseutil.SortByRevision(releases)

			if tt.expectCurrent != nil {
				g.Expect(obj.GetCurrent()).To(testutil.Equal(tt.expectCurrent(releases)))
			} else {
				g.Expect(obj.GetCurrent()).To(BeNil(), "expected current to be nil")
			}

			if tt.expectPrevious != nil {
				g.Expect(obj.GetPrevious()).To(testutil.Equal(tt.expectPrevious(releases)))
			} else {
				g.Expect(obj.GetPrevious()).To(BeNil(), "expected previous to be nil")
			}

			g.Expect(obj.Status.Failures).To(Equal(tt.expectFailures))
			g.Expect(obj.Status.InstallFailures).To(Equal(tt.expectInstallFailures))
			g.Expect(obj.Status.UpgradeFailures).To(Equal(tt.expectUpgradeFailures))
		})
	}
}

func Test_observeUninstall(t *testing.T) {
	t.Run("uninstall of current", func(t *testing.T) {
		g := NewWithT(t)

		current := &helmv2.HelmReleaseInfo{
			Name:      mockReleaseName,
			Namespace: mockReleaseNamespace,
			Version:   1,
			Status:    helmrelease.StatusDeployed.String(),
		}
		obj := &helmv2.HelmRelease{
			Status: helmv2.HelmReleaseStatus{
				Current: current,
			},
		}
		rls := testutil.BuildRelease(&helmrelease.MockReleaseOptions{
			Name:      current.Name,
			Namespace: current.Namespace,
			Version:   current.Version,
			Status:    helmrelease.StatusUninstalled,
		})
		expect := release.ObservedToInfo(release.ObserveRelease(rls))

		observeUninstall(obj)(rls)
		g.Expect(obj.GetCurrent()).ToNot(BeNil())
		g.Expect(obj.GetCurrent()).To(Equal(expect))
		g.Expect(obj.GetPrevious()).To(BeNil())
	})

	t.Run("uninstall without current", func(t *testing.T) {
		g := NewWithT(t)

		obj := &helmv2.HelmRelease{
			Status: helmv2.HelmReleaseStatus{
				Current: nil,
			},
		}
		rls := testutil.BuildRelease(&helmrelease.MockReleaseOptions{
			Name:      mockReleaseName,
			Namespace: mockReleaseNamespace,
			Version:   1,
			Status:    helmrelease.StatusUninstalling,
		})

		observeUninstall(obj)(rls)
		g.Expect(obj.GetCurrent()).To(BeNil())
		g.Expect(obj.GetPrevious()).To(BeNil())
	})

	t.Run("uninstall of different version than current", func(t *testing.T) {
		g := NewWithT(t)

		current := &helmv2.HelmReleaseInfo{
			Name:      mockReleaseName,
			Namespace: mockReleaseNamespace,
			Version:   1,
			Status:    helmrelease.StatusDeployed.String(),
		}
		obj := &helmv2.HelmRelease{
			Status: helmv2.HelmReleaseStatus{
				Current: current,
			},
		}
		rls := testutil.BuildRelease(&helmrelease.MockReleaseOptions{
			Name:      current.Name,
			Namespace: current.Namespace,
			Version:   current.Version + 1,
			Status:    helmrelease.StatusUninstalled,
		})

		observeUninstall(obj)(rls)
		g.Expect(obj.GetCurrent()).ToNot(BeNil())
		g.Expect(obj.GetCurrent()).To(Equal(current))
		g.Expect(obj.GetPrevious()).To(BeNil())
	})
}
