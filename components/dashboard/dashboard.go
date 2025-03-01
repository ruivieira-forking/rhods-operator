// Package dashboard provides utility functions to config Open Data Hub Dashboard: A web dashboard that displays
// installed Open Data Hub components with easy access to component UIs and documentation
package dashboard

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
	v1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dsciv1 "github.com/opendatahub-io/opendatahub-operator/v2/apis/dscinitialization/v1"
	"github.com/opendatahub-io/opendatahub-operator/v2/components"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/cluster"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/common"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/deploy"
	"github.com/opendatahub-io/opendatahub-operator/v2/pkg/monitoring"
)

var (
	ComponentName          = "dashboard"
	ComponentNameSupported = "rhods-dashboard"
	Path                   = deploy.DefaultManifestPath + "/" + ComponentName + "/base"
	PathSupported          = deploy.DefaultManifestPath + "/" + ComponentName + "/overlays/rhods"
	PathISVSM              = deploy.DefaultManifestPath + "/" + ComponentName + "/apps/apps-onprem"
	PathISVAddOn           = deploy.DefaultManifestPath + "/" + ComponentName + "/apps/apps-addon"
	PathOVMS               = deploy.DefaultManifestPath + "/" + ComponentName + "/modelserving"
	PathODHDashboardConfig = deploy.DefaultManifestPath + "/" + ComponentName + "/odhdashboardconfig"
	PathConsoleLink        = deploy.DefaultManifestPath + "/" + ComponentName + "/consolelink"
	PathCRDs               = deploy.DefaultManifestPath + "/" + ComponentName + "/crd"
	NameConsoleLink        = "console"
	NamespaceConsoleLink   = "openshift-console"
	PathAnaconda           = deploy.DefaultManifestPath + "/partners/anaconda/base/"
)

// Verifies that Dashboard implements ComponentInterface.
var _ components.ComponentInterface = (*Dashboard)(nil)

// Dashboard struct holds the configuration for the Dashboard component.
// +kubebuilder:object:generate=true
type Dashboard struct {
	components.Component `json:""`
}

func (d *Dashboard) OverrideManifests(platform string) error {
	// If devflags are set, update default manifests path
	if len(d.DevFlags.Manifests) != 0 {
		manifestConfig := d.DevFlags.Manifests[0]
		if err := deploy.DownloadManifests(ComponentName, manifestConfig); err != nil {
			return err
		}
		// If overlay is defined, update paths
		if platform == string(deploy.ManagedRhods) || platform == string(deploy.SelfManagedRhods) {
			defaultKustomizePath := "overlays/rhods"
			if manifestConfig.SourcePath != "" {
				defaultKustomizePath = manifestConfig.SourcePath
			}
			PathSupported = filepath.Join(deploy.DefaultManifestPath, ComponentName, defaultKustomizePath)
		} else {
			defaultKustomizePath := "base"
			if manifestConfig.SourcePath != "" {
				defaultKustomizePath = manifestConfig.SourcePath
			}
			Path = filepath.Join(deploy.DefaultManifestPath, ComponentName, defaultKustomizePath)
		}
	}
	return nil
}

func (d *Dashboard) GetComponentName() string {
	return ComponentName
}

//nolint:gocyclo
func (d *Dashboard) ReconcileComponent(ctx context.Context,
	cli client.Client,
	resConf *rest.Config,
	owner metav1.Object,
	dscispec *dsciv1.DSCInitializationSpec,
	currentComponentExist bool,
) error {
	var imageParamMap = map[string]string{
		"odh-dashboard-image": "RELATED_IMAGE_ODH_DASHBOARD_IMAGE",
	}
	enabled := d.GetManagementState() == operatorv1.Managed
	monitoringEnabled := dscispec.Monitoring.ManagementState == operatorv1.Managed

	platform, err := deploy.GetPlatform(cli)
	if err != nil {
		return err
	}

	// Update Default rolebinding
	if enabled {
		// cleanup OAuth client related secret and CR if dashboard is in 'installed falas' status
		if err := d.cleanOauthClient(cli, dscispec, currentComponentExist); err != nil {
			return err
		}
		if d.DevFlags != nil {
			// Download manifests and update paths
			if err := d.OverrideManifests(string(platform)); err != nil {
				return err
			}
		}
		if platform == deploy.OpenDataHub || platform == "" {
			err := cluster.UpdatePodSecurityRolebinding(cli, dscispec.ApplicationsNamespace, "odh-dashboard")
			if err != nil {
				return err
			}
			// Deploy CRDs
			if err := d.deployCRDsForPlatform(cli, owner, dscispec.ApplicationsNamespace, platform); err != nil {
				return fmt.Errorf("failed to deploy %s crds %s: %w", ComponentName, PathCRDs, err)
			}
		}

		if platform == deploy.SelfManagedRhods || platform == deploy.ManagedRhods {
			err := cluster.UpdatePodSecurityRolebinding(cli, dscispec.ApplicationsNamespace, "rhods-dashboard")
			if err != nil {
				return err
			}
			// Deploy CRDs
			if err := d.deployCRDsForPlatform(cli, owner, dscispec.ApplicationsNamespace, platform); err != nil {
				return fmt.Errorf("failed to deploy %s crds %s: %w", ComponentNameSupported, PathCRDs, err)
			}
			// Apply RHOAI specific configs
			if err := d.applyRhodsSpecificConfigs(cli, owner, dscispec.ApplicationsNamespace, platform); err != nil {
				return err
			}
		}

		// Update image parameters (ODH does not use this solution, only downstream)
		if (dscispec.DevFlags == nil || dscispec.DevFlags.ManifestsUri == "") && (d.DevFlags == nil || len(d.DevFlags.Manifests) == 0) {
			if err := deploy.ApplyParams(PathSupported, d.SetImageParamsMap(imageParamMap), false); err != nil {
				return err
			}
		}
	}

	// Deploy odh-dashboard manifests
	if platform == deploy.OpenDataHub || platform == "" {
		if err = deploy.DeployManifestsFromPath(cli, owner, Path, dscispec.ApplicationsNamespace, ComponentName, enabled); err != nil {
			return err
		}
	} else if platform == deploy.SelfManagedRhods || platform == deploy.ManagedRhods {
		// Apply authentication overlay
		if err := deploy.DeployManifestsFromPath(cli, owner, PathSupported, dscispec.ApplicationsNamespace, ComponentNameSupported, enabled); err != nil {
			return err
		}
	}

	switch platform {
	case deploy.SelfManagedRhods, deploy.ManagedRhods:
		sectionTitle := "OpenShift Managed Services"
		if platform == deploy.SelfManagedRhods {
			sectionTitle = "OpenShift Self Managed Services"
		}

		if err := d.deployISVManifests(cli, owner, ComponentNameSupported, dscispec.ApplicationsNamespace, platform); err != nil {
			return err
		}

		if err := d.deployConsoleLink(cli, owner, dscispec.ApplicationsNamespace, ComponentNameSupported, sectionTitle); err != nil {
			return err
		}
		// CloudService Monitoring handling
		if platform == deploy.ManagedRhods {
			if enabled {
				// first check if the service is up, so prometheus wont fire alerts when it is just startup
				if err := monitoring.WaitForDeploymentAvailable(ctx, resConf, ComponentNameSupported, dscispec.ApplicationsNamespace, 20, 3); err != nil {
					return fmt.Errorf("deployment for %s is not ready to server: %w", ComponentName, err)
				}
				fmt.Printf("deployment for %s is done, updating monitoring rules\n", ComponentNameSupported)
			}

			if err := d.UpdatePrometheusConfig(cli, enabled && monitoringEnabled, ComponentNameSupported); err != nil {
				return err
			}
			if err = deploy.DeployManifestsFromPath(cli, owner,
				filepath.Join(deploy.DefaultManifestPath, "monitoring", "prometheus", "apps"),
				dscispec.Monitoring.Namespace,
				"prometheus", true); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func (d *Dashboard) deployCRDsForPlatform(cli client.Client, owner metav1.Object, namespace string, platform deploy.Platform) error {
	componentName := ComponentName
	if platform == deploy.SelfManagedRhods || platform == deploy.ManagedRhods {
		componentName = ComponentNameSupported
	}

	enabled := d.ManagementState == operatorv1.Managed
	return deploy.DeployManifestsFromPath(cli, owner, PathCRDs, namespace, componentName, enabled)
}

func (d *Dashboard) applyRhodsSpecificConfigs(cli client.Client, owner metav1.Object, namespace string, platform deploy.Platform) error {
	dashboardConfig := filepath.Join(PathODHDashboardConfig, "odhdashboardconfig.yaml")
	adminGroups := map[deploy.Platform]string{
		deploy.SelfManagedRhods: "rhods-admins",
		deploy.ManagedRhods:     "dedicated-admins",
	}[platform]

	if err := common.ReplaceStringsInFile(dashboardConfig, map[string]string{"<admin_groups>": adminGroups}); err != nil {
		return err
	}

	enabled := d.ManagementState == operatorv1.Managed
	if err := deploy.DeployManifestsFromPath(cli, owner, PathODHDashboardConfig, namespace, ComponentNameSupported, enabled); err != nil {
		return fmt.Errorf("failed to set dashboard config from %s: %w", PathODHDashboardConfig, err)
	}

	if err := deploy.DeployManifestsFromPath(cli, owner, PathOVMS, namespace, ComponentNameSupported, enabled); err != nil {
		return fmt.Errorf("failed to set dashboard OVMS from %s: %w", PathOVMS, err)
	}

	if err := cluster.CreateSecret(cli, "anaconda-ce-access", namespace); err != nil {
		return fmt.Errorf("failed to create access-secret for anaconda: %w", err)
	}

	return deploy.DeployManifestsFromPath(cli, owner, PathAnaconda, namespace, ComponentNameSupported, enabled)
}

func (d *Dashboard) deployISVManifests(cli client.Client, owner metav1.Object, componentName, namespace string, platform deploy.Platform) error {
	var path string
	switch platform {
	case deploy.SelfManagedRhods:
		path = PathISVSM
	case deploy.ManagedRhods:
		path = PathISVAddOn
	default:
		return nil
	}

	enabled := d.ManagementState == operatorv1.Managed
	if err := deploy.DeployManifestsFromPath(cli, owner, path, namespace, componentName, enabled); err != nil {
		return fmt.Errorf("failed to set dashboard ISV from %s: %w", path, err)
	}

	return nil
}

func (d *Dashboard) deployConsoleLink(cli client.Client, owner metav1.Object, namespace, componentName, sectionTitle string) error {
	pathConsoleLink := filepath.Join(PathConsoleLink, "consolelink.yaml")

	consoleRoute := &routev1.Route{}
	if err := cli.Get(context.TODO(), client.ObjectKey{Name: NameConsoleLink, Namespace: NamespaceConsoleLink}, consoleRoute); err != nil {
		return fmt.Errorf("error getting console route URL: %w", err)
	}

	domainIndex := strings.Index(consoleRoute.Spec.Host, ".")
	consoleLinkDomain := consoleRoute.Spec.Host[domainIndex+1:]
	err := common.ReplaceStringsInFile(pathConsoleLink, map[string]string{
		"<rhods-dashboard-url>": "https://rhods-dashboard-" + namespace + "." + consoleLinkDomain,
		"<section-title>":       sectionTitle,
	})
	if err != nil {
		return fmt.Errorf("error replacing with correct dashboard url for ConsoleLink: %w", err)
	}

	enabled := d.ManagementState == operatorv1.Managed
	err = deploy.DeployManifestsFromPath(cli, owner, PathConsoleLink, namespace, componentName, enabled)
	if err != nil {
		return fmt.Errorf("failed to set dashboard consolelink from %s: %w", PathConsoleLink, err)
	}

	return nil
}

func (d *Dashboard) cleanOauthClient(cli client.Client, dscispec *dsciv1.DSCInitializationSpec, currentComponentExist bool) error {
	// Remove previous oauth-client secrets
	// Check if component is going from state of `Not Installed --> Installed`
	// Assumption: Component is currently set to enabled
	name := "dashboard-oauth-client"
	if !currentComponentExist {
		fmt.Println("Cleanup any left secret")
		// Delete client secrets from previous installation
		oauthClientSecret := &v1.Secret{}
		err := cli.Get(context.TODO(), client.ObjectKey{
			Namespace: dscispec.ApplicationsNamespace,
			Name:      name,
		}, oauthClientSecret)
		if err != nil {
			if !apierrs.IsNotFound(err) {
				return fmt.Errorf("error getting secret %s: %w", name, err)
			}
		} else {
			if err := cli.Delete(context.TODO(), oauthClientSecret); err != nil {
				return fmt.Errorf("error deleting oauth client secret: %w", err)
			}
		}
	}
	return nil
}
