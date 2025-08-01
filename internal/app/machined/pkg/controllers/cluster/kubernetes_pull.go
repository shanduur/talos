// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package cluster

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/internal/pkg/discovery/registry"
	"github.com/siderolabs/talos/pkg/conditions"
	"github.com/siderolabs/talos/pkg/kubernetes"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/cluster"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
)

// KubernetesPullController pulls list of Affiliate resource from the Kubernetes registry.
type KubernetesPullController struct{}

// Name implements controller.Controller interface.
func (ctrl *KubernetesPullController) Name() string {
	return "cluster.KubernetesPullController"
}

// Inputs implements controller.Controller interface.
func (ctrl *KubernetesPullController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: config.NamespaceName,
			Type:      cluster.ConfigType,
			ID:        optional.Some(cluster.ConfigID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: k8s.NamespaceName,
			Type:      k8s.NodenameType,
			ID:        optional.Some(k8s.NodenameID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *KubernetesPullController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: cluster.AffiliateType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo,cyclop
func (ctrl *KubernetesPullController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	var (
		kubernetesClient   *kubernetes.Client
		kubernetesRegistry *registry.Kubernetes
		watchCtxCancel     context.CancelFunc
		notifyCh           <-chan struct{}
		notifyCloser       func()
	)

	defer func() {
		if watchCtxCancel != nil {
			watchCtxCancel()
		}

		if notifyCloser != nil {
			notifyCloser()
		}

		if kubernetesClient != nil {
			kubernetesClient.Close() //nolint:errcheck
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-notifyCh:
		}

		discoveryConfig, err := safe.ReaderGetByID[*cluster.Config](ctx, r, cluster.ConfigID)
		if err != nil {
			if !state.IsNotFoundError(err) {
				return fmt.Errorf("error getting discovery config: %w", err)
			}

			continue
		}

		if !discoveryConfig.TypedSpec().RegistryKubernetesEnabled {
			// if discovery is disabled cleanup existing resources
			if err = cleanupAffiliates(ctx, ctrl, r, nil); err != nil {
				return err
			}

			continue
		}

		if err = conditions.WaitForKubeconfigReady(constants.KubeletKubeconfig).Wait(ctx); err != nil {
			return err
		}

		nodename, err := safe.ReaderGetByID[*k8s.Nodename](ctx, r, k8s.NodenameID)
		if err != nil {
			if !state.IsNotFoundError(err) {
				return fmt.Errorf("error getting nodename: %w", err)
			}

			continue
		}

		if kubernetesClient == nil {
			kubernetesClient, err = kubernetes.NewClientFromKubeletKubeconfig()
			if err != nil {
				return fmt.Errorf("error building kubernetes client: %w", err)
			}
		}

		if kubernetesRegistry == nil {
			kubernetesRegistry = registry.NewKubernetes(kubernetesClient)
		}

		if notifyCh == nil {
			var watchCtx context.Context

			watchCtx, watchCtxCancel = context.WithCancel(ctx) //nolint:govet

			notifyCh, notifyCloser, err = kubernetesRegistry.Watch(watchCtx, logger)
			if err != nil {
				return fmt.Errorf("error setting up registry watcher: %w", err) //nolint:govet
			}
		}

		affiliateSpecs, err := kubernetesRegistry.List(nodename.TypedSpec().Nodename)
		if err != nil {
			return fmt.Errorf("error listing affiliates: %w", err)
		}

		touchedIDs := make(map[resource.ID]struct{})

		for _, affilateSpec := range affiliateSpecs {
			id := fmt.Sprintf("k8s/%s", affilateSpec.NodeID)

			if err = safe.WriterModify(ctx, r, cluster.NewAffiliate(cluster.RawNamespaceName, id), func(res *cluster.Affiliate) error {
				*res.TypedSpec() = *affilateSpec

				return nil
			}); err != nil {
				return err
			}

			touchedIDs[id] = struct{}{}
		}

		if err := cleanupAffiliates(ctx, ctrl, r, touchedIDs); err != nil {
			return err
		}

		r.ResetRestartBackoff()
	}
}
