package packager2

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/defenseunicorns/pkg/helpers/v2"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zarf-dev/zarf/src/api/v1alpha1"
	"github.com/zarf-dev/zarf/src/config"
	"github.com/zarf-dev/zarf/src/internal/healthchecks"
	"github.com/zarf-dev/zarf/src/internal/packager/helm"
	"github.com/zarf-dev/zarf/src/internal/packager/template"
	"github.com/zarf-dev/zarf/src/pkg/cluster"
	"github.com/zarf-dev/zarf/src/pkg/layout"
	"github.com/zarf-dev/zarf/src/pkg/logger"
	"github.com/zarf-dev/zarf/src/pkg/message"
	"github.com/zarf-dev/zarf/src/pkg/packager/actions"
	"github.com/zarf-dev/zarf/src/pkg/packager/filters"
	"github.com/zarf-dev/zarf/src/types"
)

// localClusterServiceRegex is used to match the local cluster service format:
var localClusterServiceRegex = regexp.MustCompile(`^(?P<name>[^\.]+)\.(?P<namespace>[^\.]+)\.svc\.cluster\.local$`)

type DeployOptions struct {
	OptionalComponents string
}

func Deploy(ctx context.Context, opt DeployOptions) error {
	l := logger.From(ctx)
	start := time.Now()
	isInteractive := !config.CommonOptions.Confirm

	deployFilter := filters.Combine(
		filters.ByLocalOS(runtime.GOOS),
		filters.ForDeploy(opt.OptionalComponents, isInteractive),
	)

	var pkg v1alpha1.ZarfPackage
	warnings := []string{}
	if isInteractive {
		filter := filters.Empty()
		pkg, loadWarnings, err := p.source.LoadPackage(ctx, p.layout, filter, true)
		if err != nil {
			return fmt.Errorf("unable to load the package: %w", err)
		}
		warnings = append(warnings, loadWarnings...)
	} else {
		pkg, loadWarnings, err := p.source.LoadPackage(ctx, p.layout, deployFilter, true)
		if err != nil {
			return fmt.Errorf("unable to load the package: %w", err)
		}
		warnings = append(warnings, loadWarnings...)
		if err := p.populatePackageVariableConfig(); err != nil {
			return fmt.Errorf("unable to set the active variables: %w", err)
		}
	}

	// validateWarnings, err := validateLastNonBreakingVersion(config.CLIVersion, pkg.Build.LastNonBreakingVersion)
	// if err != nil {
	// 	return err
	// }
	// warnings = append(warnings, validateWarnings...)
	// for _, warning := range validateWarnings {
	// 	l.Warn(warning)
	// }

	// sbomViewFiles, sbomWarnings, err := p.layout.SBOMs.StageSBOMViewFiles()
	// if err != nil {
	// 	return err
	// }
	// warnings = append(warnings, sbomWarnings...)
	// for _, warning := range sbomWarnings {
	// 	l.Warn(warning)
	// }

	// Confirm the overall package deployment
	// if !p.confirmAction(ctx, config.ZarfDeployStage, warnings, sbomViewFiles) {
	// 	return fmt.Errorf("deployment cancelled")
	// }

	// if isInteractive {
	// 	pkg.Components, err = deployFilter.Apply(p.cfg.Pkg)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	// Set variables and prompt if --confirm is not set
	// 	if err := p.populatePackageVariableConfig(); err != nil {
	// 		return fmt.Errorf("unable to set the active variables: %w", err)
	// 	}
	// }

	// p.hpaModified = false
	// Reset registry HPA scale down whether an error occurs or not
	// defer p.resetRegistryHPA(ctx)

	// Get a list of all the components we are deploying and actually deploy them
	deployedComponents, err := deployPackage(ctx, pkg)
	if err != nil {
		return err
	}
	if len(deployedComponents) == 0 {
		message.Warn("No components were selected for deployment.  Inspect the package to view the available components and select components interactively or by name with \"--components\"")
		l.Warn("no components were selected for deployment. Inspect the package to view the available components and select components interactively or by name with \"--components\"")
	}

	// Notify all the things about the successful deployment
	message.Successf("Zarf deployment complete")
	l.Debug("Zarf deployment complete", "duration", time.Since(start))

	// err = p.printTablesForDeployment(ctx, deployedComponents)
	// if err != nil {
	// 	return err
	// }

	return nil
}

// deployPackage loops through a list of ZarfComponents and deploys them.
func deployPackage(ctx context.Context, pkg v1alpha1.ZarfPackage) ([]types.DeployedComponent, error) {
	l := logger.From(ctx)

	var c *cluster.Cluster
	var state *types.ZarfState
	deployedComponents := []types.DeployedComponent{}
	for _, component := range pkg.Components {
		if component.RequiresCluster() {
			timeout := cluster.DefaultTimeout
			if pkg.IsInitConfig() {
				timeout = 5 * time.Minute
			}
			connectCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cluster, err := cluster.NewClusterWithWait(connectCtx)
			if err != nil {
				return nil, err
			}
			c = cluster
			err = attemptClusterChecks(ctx, c)
			if err != nil {
				return nil, err
			}

			if pkg.IsInitConfig() && state != nil {
				// err := c.InitZarfState(ctx, p.cfg.InitOpts)
				// if err != nil {
				// 	return nil, err
				// }
			}
			if state == nil {
				state, err = setupState(ctx, c, pkg)
				if err != nil {
					return nil, err
				}
			}
		}

		deployedComponent := types.DeployedComponent{
			Name: component.Name,
		}

		// Ensure we don't overwrite any installedCharts data when updating the package secret
		if c != nil {
			installedCharts, err := c.GetInstalledChartsForComponent(ctx, pkg.Metadata.Name, component)
			if err != nil {
				message.Debugf("Unable to fetch installed Helm charts for component '%s': %s", component.Name, err.Error())
				l.Debug("unable to fetch installed Helm charts", "component", component.Name, "error", err.Error())
			}
			deployedComponent.InstalledCharts = installedCharts
		}

		deployedComponents = append(deployedComponents, deployedComponent)
		idx := len(deployedComponents) - 1

		variableConfig := template.GetZarfVariableConfig(ctx)

		// Deploy the component
		var charts []types.InstalledChart
		var deployErr error
		if pkg.IsInitConfig() {
			charts, deployErr = deployInitComponent(ctx, c, state, component)
		} else {
			charts, deployErr = deployComponent(ctx, c, state, component, false, false)
		}

		onFailure := func() {
			if err := actions.Run(ctx, component.Actions.OnDeploy.Defaults, component.Actions.OnDeploy.OnFailure, variableConfig); err != nil {
				message.Debugf("unable to run component failure action: %s", err.Error())
				l.Debug("unable to run component failure action", "error", err.Error())
			}
		}

		if deployErr != nil {
			onFailure()
			if c != nil {
				if _, err := c.RecordPackageDeployment(ctx, pkg, deployedComponents); err != nil {
					message.Debugf("Unable to record package deployment for component %q: this will affect features like `zarf package remove`: %s", component.Name, err.Error())
					l.Debug("unable to record package deployment", "component", component.Name, "error", err.Error())
				}
			}
			return nil, fmt.Errorf("unable to deploy component %q: %w", component.Name, deployErr)
		}

		// Update the package secret to indicate that we successfully deployed this component
		deployedComponents[idx].InstalledCharts = charts
		if c != nil {
			if _, err := c.RecordPackageDeployment(ctx, pkg, deployedComponents); err != nil {
				message.Debugf("Unable to record package deployment for component %q: this will affect features like `zarf package remove`: %s", component.Name, err.Error())
				l.Debug("unable to record package deployment", "component", component.Name, "error", err.Error())
			}
		}

		if err := actions.Run(ctx, component.Actions.OnDeploy.Defaults, component.Actions.OnDeploy.OnSuccess, variableConfig); err != nil {
			onFailure()
			return nil, fmt.Errorf("unable to run component success action: %w", err)
		}
	}

	return deployedComponents, nil
}

func deployInitComponent(ctx context.Context, c *cluster.Cluster, state *types.ZarfState, component v1alpha1.ZarfComponent) ([]types.InstalledChart, error) {
	l := logger.From(ctx)
	hasExternalRegistry := p.cfg.InitOpts.RegistryInfo.Address != ""
	isSeedRegistry := component.Name == "zarf-seed-registry"
	isRegistry := component.Name == "zarf-registry"
	isInjector := component.Name == "zarf-injector"
	isAgent := component.Name == "zarf-agent"
	isK3s := component.Name == "k3s"

	if isK3s {
		p.cfg.InitOpts.ApplianceMode = true
	}

	if hasExternalRegistry && (isSeedRegistry || isInjector || isRegistry) {
		message.Notef("Not deploying the component (%s) since external registry information was provided during `zarf init`", component.Name)
		l.Info("skipping init package component since external registry information was provided", "component", component.Name)
		return nil, nil
	}

	// if isRegistry {
	// 	// If we are deploying the registry then mark the HPA as "modified" to set it to Min later
	// 	p.hpaModified = true
	// }

	// Before deploying the seed registry, start the injector
	if isSeedRegistry {
		err := c.StartInjection(ctx, p.layout.Base, p.layout.Images.Base, component.Images)
		if err != nil {
			return nil, err
		}
	}

	// Skip image checksum if component is agent.
	// Skip image push if component is seed registry.
	charts, err := deployComponent(ctx, c, state, component, isAgent, isSeedRegistry)
	if err != nil {
		return nil, err
	}

	// Do cleanup for when we inject the seed registry during initialization
	if isSeedRegistry {
		if err := c.StopInjection(ctx); err != nil {
			return nil, fmt.Errorf("failed to delete injector resources: %w", err)
		}
	}

	return charts, nil
}

// Deploy a Zarf Component.
func deployComponent(ctx context.Context, c *cluster.Cluster, state *types.ZarfState, component v1alpha1.ZarfComponent, noImgChecksum bool, noImgPush bool) ([]types.InstalledChart, error) {
	l := logger.From(ctx)
	start := time.Now()
	// Toggles for general deploy operations
	componentPath := p.layout.Components.Dirs[component.Name]

	message.HeaderInfof("📦 %s COMPONENT", strings.ToUpper(component.Name))
	l.Info("deploying component", "name", component.Name)

	// hasImages := len(component.Images) > 0 && !noImgPush
	// hasCharts := len(component.Charts) > 0
	// hasManifests := len(component.Manifests) > 0
	// hasRepos := len(component.Repos) > 0
	// hasFiles := len(component.Files) > 0

	// 	// Disable the registry HPA scale down if we are deploying images and it is not already disabled
	// 	if hasImages && !p.hpaModified && p.state.RegistryInfo.IsInternal() {
	// 		if err := p.cluster.DisableRegHPAScaleDown(ctx); err != nil {
	// 			message.Debugf("unable to disable the registry HPA scale down: %s", err.Error())
	// 			l.Debug("unable to disable the registry HPA scale down", "error", err.Error())
	// 		} else {
	// 			p.hpaModified = true
	// 		}
	// 	}
	// }

	// err := p.populateComponentAndStateTemplates(ctx, component.Name)
	// if err != nil {
	// 	return nil, err
	// }

	if err := actions.Run(ctx, component.Actions.OnDeploy.Defaults, component.Actions.OnDeploy.Before, p.variableConfig); err != nil {
		return nil, fmt.Errorf("unable to run component before action: %w", err)
	}

	// if hasFiles {
	// 	if err := p.processComponentFiles(ctx, component, componentPath.Files); err != nil {
	// 		return nil, fmt.Errorf("unable to process the component files: %w", err)
	// 	}
	// }

	// if hasImages {
	// 	if err := p.pushImagesToRegistry(ctx, component.Images, noImgChecksum); err != nil {
	// 		return nil, fmt.Errorf("unable to push images to the registry: %w", err)
	// 	}
	// }

	// if hasRepos {
	// 	if err = p.pushReposToRepository(ctx, componentPath.Repos, component.Repos); err != nil {
	// 		return nil, fmt.Errorf("unable to push the repos to the repository: %w", err)
	// 	}
	// }

	g, gCtx := errgroup.WithContext(ctx)
	for idx, data := range component.DataInjections {
		g.Go(func() error {
			return c.HandleDataInjection(gCtx, data, componentPath, idx)
		})
	}

	charts := []types.InstalledChart{}
	// if hasCharts || hasManifests {
	// 	charts, err = p.installChartAndManifests(ctx, componentPath, component)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }

	if err := actions.Run(ctx, component.Actions.OnDeploy.Defaults, component.Actions.OnDeploy.After, p.variableConfig); err != nil {
		return nil, fmt.Errorf("unable to run component after action: %w", err)
	}

	if len(component.HealthChecks) > 0 {
		healthCheckContext, cancel := context.WithTimeout(ctx, p.cfg.DeployOpts.Timeout)
		defer cancel()
		spinner := message.NewProgressSpinner("Running health checks")
		l.Info("running health checks")
		defer spinner.Stop()
		if err := healthchecks.Run(healthCheckContext, c.Watcher, component.HealthChecks); err != nil {
			return nil, fmt.Errorf("health checks failed: %w", err)
		}
		spinner.Success()
	}

	err := g.Wait()
	if err != nil {
		return nil, err
	}
	l.Debug("done deploying component", "name", component.Name, "duration", time.Since(start))
	return charts, nil
}

// Move files onto the host of the machine performing the deployment.
func processComponentFiles(ctx context.Context, component v1alpha1.ZarfComponent, pkgLocation string) error {
	l := logger.From(ctx)
	spinner := message.NewProgressSpinner("Copying %d files", len(component.Files))
	start := time.Now()
	l.Info("copying files", "count", len(component.Files))
	defer spinner.Stop()

	for fileIdx, file := range component.Files {
		spinner.Updatef("Loading %s", file.Target)
		l.Info("loading file", "name", file.Target)

		fileLocation := filepath.Join(pkgLocation, strconv.Itoa(fileIdx), filepath.Base(file.Target))
		if helpers.InvalidPath(fileLocation) {
			fileLocation = filepath.Join(pkgLocation, strconv.Itoa(fileIdx))
		}

		// If a shasum is specified check it again on deployment as well
		if file.Shasum != "" {
			spinner.Updatef("Validating SHASUM for %s", file.Target)
			l.Debug("Validating SHASUM", "file", file.Target)
			if err := helpers.SHAsMatch(fileLocation, file.Shasum); err != nil {
				return err
			}
		}

		// Replace temp target directory and home directory
		target, err := config.GetAbsHomePath(strings.Replace(file.Target, "###ZARF_TEMP###", p.layout.Base, 1))
		if err != nil {
			return err
		}
		file.Target = target

		fileList := []string{}
		if helpers.IsDir(fileLocation) {
			files, _ := helpers.RecursiveFileList(fileLocation, nil, false)
			fileList = append(fileList, files...)
		} else {
			fileList = append(fileList, fileLocation)
		}

		for _, subFile := range fileList {
			// Check if the file looks like a text file
			isText, err := helpers.IsTextFile(subFile)
			if err != nil {
				return err
			}

			// If the file is a text file, template it
			if isText {
				spinner.Updatef("Templating %s", file.Target)
				l.Debug("template file", "name", file.Target)
				if err := p.variableConfig.ReplaceTextTemplate(subFile); err != nil {
					return fmt.Errorf("unable to template file %s: %w", subFile, err)
				}
			}
		}

		// Copy the file to the destination
		spinner.Updatef("Saving %s", file.Target)
		l.Debug("saving file", "name", file.Target)
		err = helpers.CreatePathAndCopy(fileLocation, file.Target)
		if err != nil {
			return fmt.Errorf("unable to copy file %s to %s: %w", fileLocation, file.Target, err)
		}

		// Loop over all symlinks and create them
		for _, link := range file.Symlinks {
			spinner.Updatef("Adding symlink %s->%s", link, file.Target)
			// Try to remove the filepath if it exists
			_ = os.RemoveAll(link)
			// Make sure the parent directory exists
			_ = helpers.CreateParentDirectory(link)
			// Create the symlink
			err := os.Symlink(file.Target, link)
			if err != nil {
				return fmt.Errorf("unable to create symlink %s->%s: %w", link, file.Target, err)
			}
		}

		// Cleanup now to reduce disk pressure
		_ = os.RemoveAll(fileLocation)
	}

	spinner.Success()
	l.Debug("done copying files", "duration", time.Since(start))

	return nil
}

// setupState fetches the current ZarfState from the k8s cluster and sets the packager to use it
func setupState(ctx context.Context, c *cluster.Cluster, pkg v1alpha1.ZarfPackage) (*types.ZarfState, error) {
	l := logger.From(ctx)

	l.Debug("loading the Zarf State from the Kubernetes cluster")

	state, err := c.LoadZarfState(ctx)
	// We ignore the error if in YOLO mode because Zarf should not be initiated.
	if err != nil && !pkg.Metadata.YOLO {
		return nil, err
	}
	// Only ignore state load error in yolo mode when secret could not be found.
	if err != nil && !kerrors.IsNotFound(err) && pkg.Metadata.YOLO {
		return nil, err
	}
	if state == nil && pkg.Metadata.YOLO {
		state = &types.ZarfState{}
		// YOLO mode, so minimal state needed
		state.Distro = "YOLO"

		l.Info("creating the Zarf namespace")
		zarfNamespace := cluster.NewZarfManagedApplyNamespace(cluster.ZarfNamespaceName)
		_, err = c.Clientset.CoreV1().Namespaces().Apply(ctx, zarfNamespace, metav1.ApplyOptions{Force: true, FieldManager: cluster.FieldManagerName})
		if err != nil {
			return nil, fmt.Errorf("unable to apply the Zarf namespace: %w", err)
		}
	}

	if pkg.Metadata.YOLO && state.Distro != "YOLO" {
		message.Warn("This package is in YOLO mode, but the cluster was already initialized with 'zarf init'. " +
			"This may cause issues if the package does not exclude any charts or manifests from the Zarf Agent using " +
			"the pod or namespace label `zarf.dev/agent: ignore'.")
		l.Warn("This package is in YOLO mode, but the cluster was already initialized with 'zarf init'. " +
			"This may cause issues if the package does not exclude any charts or manifests from the Zarf Agent using " +
			"the pod or namespace label `zarf.dev/agent: ignore'.")
	}

	return state, nil
}

// func (p *Packager) populateComponentAndStateTemplates(ctx context.Context, componentName string) error {
// 	applicationTemplates, err := template.GetZarfTemplates(ctx, componentName, p.state)
// 	if err != nil {
// 		return err
// 	}
// 	p.variableConfig.SetApplicationTemplates(applicationTemplates)
// 	return nil
// }

// func (p *Packager) populatePackageVariableConfig() error {
// 	p.variableConfig.SetConstants(p.cfg.Pkg.Constants)
// 	return p.variableConfig.PopulateVariables(p.cfg.Pkg.Variables, p.cfg.PkgOpts.SetVariables)
// }

// generateValuesOverrides creates a map containing overrides for chart values based on the chart and component
// Specifically it merges DeployOpts.ValuesOverridesMap over Zarf `variables` for a given component/chart combination
func generateValuesOverrides(chart v1alpha1.ZarfChart, componentName string) (map[string]any, error) {
	valuesOverrides := make(map[string]any)
	chartOverrides := make(map[string]any)

	for _, variable := range chart.Variables {
		if setVar, ok := p.variableConfig.GetSetVariable(variable.Name); ok && setVar != nil {
			// Use the variable's path as a key to ensure unique entries for variables with the same name but different paths.
			if err := helpers.MergePathAndValueIntoMap(chartOverrides, variable.Path, setVar.Value); err != nil {
				return nil, fmt.Errorf("unable to merge path and value into map: %w", err)
			}
		}
	}

	// Apply any direct overrides specified in the deployment options for this component and chart
	if componentOverrides, ok := p.cfg.DeployOpts.ValuesOverridesMap[componentName]; ok {
		if chartSpecificOverrides, ok := componentOverrides[chart.Name]; ok {
			valuesOverrides = chartSpecificOverrides
		}
	}

	// Merge chartOverrides into valuesOverrides to ensure all overrides are applied.
	// This corrects the logic to ensure that chartOverrides and valuesOverrides are merged correctly.
	return helpers.MergeMapRecursive(chartOverrides, valuesOverrides), nil
}

// Install all Helm charts and raw k8s manifests into the k8s cluster.
func installChartAndManifests(ctx context.Context, componentPaths *layout.ComponentPaths, component v1alpha1.ZarfComponent) ([]types.InstalledChart, error) {
	installedCharts := []types.InstalledChart{}

	for _, chart := range component.Charts {
		// Do not wait for the chart to be ready if data injections are present.
		if len(component.DataInjections) > 0 {
			chart.NoWait = true
		}

		// zarf magic for the value file
		for idx := range chart.ValuesFiles {
			valueFilePath := helm.StandardValuesName(componentPaths.Values, chart, idx)
			if err := p.variableConfig.ReplaceTextTemplate(valueFilePath); err != nil {
				return nil, err
			}
		}

		// Create a Helm values overrides map from set Zarf `variables` and DeployOpts library inputs
		// Values overrides are to be applied in order of Helm Chart Defaults -> Zarf `valuesFiles` -> Zarf `variables` -> DeployOpts overrides
		valuesOverrides, err := p.generateValuesOverrides(chart, component.Name)
		if err != nil {
			return nil, err
		}

		helmCfg := helm.New(
			chart,
			componentPaths.Charts,
			componentPaths.Values,
			helm.WithDeployInfo(
				p.cfg,
				p.variableConfig,
				p.state,
				p.cluster,
				valuesOverrides,
				p.cfg.DeployOpts.Timeout,
				p.cfg.PkgOpts.Retries),
		)

		connectStrings, installedChartName, err := helmCfg.InstallOrUpgradeChart(ctx)
		if err != nil {
			return nil, err
		}
		installedCharts = append(installedCharts, types.InstalledChart{Namespace: chart.Namespace, ChartName: installedChartName, ConnectStrings: connectStrings})
	}

	for _, manifest := range component.Manifests {
		for idx := range manifest.Files {
			if helpers.InvalidPath(filepath.Join(componentPaths.Manifests, manifest.Files[idx])) {
				// The path is likely invalid because of how we compose OCI components, add an index suffix to the filename
				manifest.Files[idx] = fmt.Sprintf("%s-%d.yaml", manifest.Name, idx)
				if helpers.InvalidPath(filepath.Join(componentPaths.Manifests, manifest.Files[idx])) {
					return nil, fmt.Errorf("unable to find manifest file %s", manifest.Files[idx])
				}
			}
		}
		// Move kustomizations to files now
		for idx := range manifest.Kustomizations {
			kustomization := fmt.Sprintf("kustomization-%s-%d.yaml", manifest.Name, idx)
			manifest.Files = append(manifest.Files, kustomization)
		}

		if manifest.Namespace == "" {
			// Helm gets sad when you don't provide a namespace even though we aren't using helm templating
			manifest.Namespace = corev1.NamespaceDefault
		}

		// Create a chart and helm cfg from a given Zarf Manifest.
		helmCfg, err := helm.NewFromZarfManifest(
			manifest,
			componentPaths.Manifests,
			p.cfg.Pkg.Metadata.Name,
			component.Name,
			helm.WithDeployInfo(
				p.cfg,
				p.variableConfig,
				p.state,
				p.cluster,
				nil,
				p.cfg.DeployOpts.Timeout,
				p.cfg.PkgOpts.Retries),
		)
		if err != nil {
			return nil, err
		}

		// Install the chart.
		connectStrings, installedChartName, err := helmCfg.InstallOrUpgradeChart(ctx)
		if err != nil {
			return nil, err
		}
		installedCharts = append(installedCharts, types.InstalledChart{Namespace: manifest.Namespace, ChartName: installedChartName, ConnectStrings: connectStrings})
	}

	return installedCharts, nil
}

// TODO once deploy is refactored to load the Zarf package and cluster objects in the cmd package
// table printing should be moved to cmd
// func printTablesForDeployment(ctx context.Context, componentsToDeploy []types.DeployedComponent) error {
// 	// If not init config, print the application connection table
// 	if !p.cfg.Pkg.IsInitConfig() {
// 		connectStrings := types.ConnectStrings{}
// 		for _, comp := range componentsToDeploy {
// 			for _, chart := range comp.InstalledCharts {
// 				for k, v := range chart.ConnectStrings {
// 					connectStrings[k] = v
// 				}
// 			}
// 		}
// 		message.PrintConnectStringTable(connectStrings)
// 		return nil
// 	}
// 	// Don't print if cluster is not configured
// 	if p.cluster == nil {
// 		return nil
// 	}
// 	// Grab a fresh copy of the state to print the most up-to-date version of the creds
// 	latestState, err := p.cluster.LoadZarfState(ctx)
// 	if err != nil {
// 		return err
// 	}
// 	message.PrintCredentialTable(latestState, componentsToDeploy)
// 	return nil
// }

// ServiceInfoFromServiceURL takes a serviceURL and parses it to find the service info for connecting to the cluster. The string is expected to follow the following format:
// Example serviceURL: http://{SERVICE_NAME}.{NAMESPACE}.svc.cluster.local:{PORT}.
func serviceInfoFromServiceURL(serviceURL string) (string, string, int, error) {
	parsedURL, err := url.Parse(serviceURL)
	if err != nil {
		return "", "", 0, err
	}

	// Get the remote port from the serviceURL.
	remotePort, err := strconv.Atoi(parsedURL.Port())
	if err != nil {
		return "", "", 0, err
	}

	// Match hostname against local cluster service format.
	get, err := helpers.MatchRegex(localClusterServiceRegex, parsedURL.Hostname())

	// If incomplete match, return an error.
	if err != nil {
		return "", "", 0, err
	}
	return get("namespace"), get("name"), remotePort, nil
}

func attemptClusterChecks(ctx context.Context, cluster *cluster.Cluster) error {
	// spinner := message.NewProgressSpinner("Gathering additional cluster information (if available)")
	// defer spinner.Stop()

	// // Check the clusters architecture matches the package spec
	// if err := p.validatePackageArchitecture(ctx); err != nil {
	// 	if errors.Is(err, lang.ErrUnableToCheckArch) {
	// 		message.Warnf("Unable to validate package architecture: %s", err.Error())
	// 		logger.From(ctx).Warn("unable to validate package architecture", "error", err)
	// 	} else {
	// 		return err
	// 	}
	// }

	// // Check for any breaking changes between the initialized Zarf version and this CLI
	// if existingInitPackage, _ := p.cluster.GetDeployedPackage(ctx, "init"); existingInitPackage != nil {
	// 	// Use the build version instead of the metadata since this will support older Zarf versions
	// 	err := deprecated.PrintBreakingChanges(os.Stderr, existingInitPackage.Data.Build.Version, config.CLIVersion)
	// 	if err != nil {
	// 		return err
	// 	}
	// }

	// spinner.Success()

	return nil
}

// func (p *Packager) validatePackageArchitecture(ctx context.Context) error {
// 	// Ignore this check if we don't have a cluster connection, or the package contains no images
// 	if !p.isConnectedToCluster() || !p.cfg.Pkg.HasImages() {
// 		return nil
// 	}

// 	// Get node architectures
// 	nodeList, err := p.cluster.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
// 	if err != nil {
// 		return lang.ErrUnableToCheckArch
// 	}
// 	if len(nodeList.Items) == 0 {
// 		return lang.ErrUnableToCheckArch
// 	}
// 	archMap := map[string]bool{}
// 	for _, node := range nodeList.Items {
// 		archMap[node.Status.NodeInfo.Architecture] = true
// 	}
// 	architectures := []string{}
// 	for arch := range archMap {
// 		architectures = append(architectures, arch)
// 	}

// 	// Check if the package architecture and the cluster architecture are the same.
// 	if !slices.Contains(architectures, p.cfg.Pkg.Metadata.Architecture) {
// 		return fmt.Errorf(lang.CmdPackageDeployValidateArchitectureErr, p.cfg.Pkg.Metadata.Architecture, strings.Join(architectures, ", "))
// 	}

// 	return nil
// }
