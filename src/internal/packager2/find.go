// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

package packager2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"

	"github.com/defenseunicorns/pkg/helpers/v2"
	"github.com/google/go-containerregistry/pkg/crane"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/zarf-dev/zarf/src/config"
	"github.com/zarf-dev/zarf/src/internal/packager/helm"
	"github.com/zarf-dev/zarf/src/internal/packager/images"
	"github.com/zarf-dev/zarf/src/internal/packager/template"
	layout2 "github.com/zarf-dev/zarf/src/internal/packager2/layout"
	"github.com/zarf-dev/zarf/src/pkg/logger"
	"github.com/zarf-dev/zarf/src/pkg/message"
	"github.com/zarf-dev/zarf/src/pkg/utils"
	"github.com/zarf-dev/zarf/src/types"
)

var imageCheck = regexp.MustCompile(`(?mi)"image":"([^"]+)"`)
var imageFuzzyCheck = regexp.MustCompile(`(?mi)["|=]([a-z0-9\-.\/:]+:[\w.\-]*[a-z\.\-][\w.\-]*)"`)

type FindImagesOptions struct {
	RepoHelmChartPath   string
	RegistryURL         string
	KubeVersionOverride string
	CreateSetVariables  map[string]string
	DeploySetVariables  map[string]string
	SkipCosign          bool
	Flavor              string
}

func FindImages(ctx context.Context, buildPath string, opt FindImagesOptions) (map[string][]string, error) {
	l := logger.From(ctx)

	tmpDir, err := utils.MakeTempDir(config.CommonOptions.TempDirectory)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpDir)

	createOpts := layout2.CreateOptions{
		Flavor:       opt.Flavor,
		SetVariables: opt.CreateSetVariables,
	}
	pkgLayout, err := layout2.CreateSkeleton(ctx, buildPath, createOpts)
	if err != nil {
		return nil, err
	}
	defer pkgLayout.Cleanup()

	registryInfo := types.RegistryInfo{Address: opt.RegistryURL}
	err = registryInfo.FillInEmptyValues()
	if err != nil {
		return nil, err
	}
	gitServer := types.GitServerInfo{}
	err = gitServer.FillInEmptyValues()
	if err != nil {
		return nil, err
	}
	artifactServer := types.ArtifactServerInfo{}
	artifactServer.FillInEmptyValues()
	state := &types.ZarfState{
		RegistryInfo:   registryInfo,
		GitServer:      gitServer,
		ArtifactServer: artifactServer,
	}

	foundComponentImages := map[string][]string{}
	for _, comp := range pkgLayout.Pkg.Components {
		variableConfig := template.GetZarfVariableConfig(ctx)
		variableConfig.SetConstants(pkgLayout.Pkg.Constants)
		variableConfig.PopulateVariables(pkgLayout.Pkg.Variables, opt.DeploySetVariables)
		applicationTemplates, err := template.GetZarfTemplates(ctx, comp.Name, state)
		if err != nil {
			return nil, err
		}
		variableConfig.SetApplicationTemplates(applicationTemplates)

		resources := []*unstructured.Unstructured{}

		if len(comp.Charts) > 0 {
			chartsPath, err := pkgLayout.GetComponentDir(tmpDir, comp.Name, layout2.ChartsComponentDir)
			if err != nil {
				return nil, err
			}
			valuesPath, err := pkgLayout.GetComponentDir(tmpDir, comp.Name, layout2.ValuesComponentDir)
			if err != nil {
				return nil, err
			}
			for _, chart := range comp.Charts {
				helmCfg := helm.New(
					chart,
					chartsPath,
					valuesPath,
					helm.WithKubeVersion(opt.KubeVersionOverride),
					helm.WithVariableConfig(variableConfig),
				)
				err = helmCfg.PackageChart(ctx, "")
				if err != nil {
					return nil, fmt.Errorf("unable to package the chart %s: %w", chart.Name, err)
				}

				valuesFilePaths, err := helpers.RecursiveFileList(valuesPath, nil, false)
				// TODO: The values path should exist if the path is set, otherwise it should be empty.
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				for _, valueFilePath := range valuesFilePaths {
					err := variableConfig.ReplaceTextTemplate(valueFilePath)
					if err != nil {
						return nil, err
					}
				}

				chartTemplate, chartValues, err := helmCfg.TemplateChart(ctx)
				if err != nil {
					return nil, fmt.Errorf("could not render the Helm template for chart %s: %w", chart.Name, err)
				}

				// Break the template into separate resources
				yamls, err := utils.SplitYAML([]byte(chartTemplate))
				if err != nil {
					return nil, err
				}
				resources = append(resources, yamls...)

				chartTarball := helm.StandardName(chartsPath, chart) + ".tgz"
				annotatedImages, err := helm.FindAnnotatedImagesForChart(chartTarball, chartValues)
				if err != nil {
					return nil, fmt.Errorf("could not look up image annotations for chart URL %s: %w", chart.URL, err)
				}
				for _, image := range annotatedImages {
					foundComponentImages[comp.Name] = append(foundComponentImages[comp.Name], image)
				}
			}
		}

		if len(comp.Manifests) > 0 {
			manifestsPath, err := pkgLayout.GetComponentDir(tmpDir, comp.Name, layout2.ManifestsComponentDir)
			if err != nil {
				return nil, err
			}
			err = filepath.Walk(manifestsPath, func(path string, fi os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if fi.IsDir() {
					return nil
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				yamls, err := utils.SplitYAML(b)
				if err != nil {
					return err
				}
				resources = append(resources, yamls...)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}

		foundImages := foundComponentImages[comp.Name]
		for _, resource := range resources {
			imgs, regexImgs, err := getUnstructuredImages(resource)
			if err != nil {
				return nil, fmt.Errorf("could not process the Kubernetes resource %s: %w", resource.GetName(), err)
			}
			for _, regexImg := range regexImgs {
				if slices.Contains(imgs, regexImg) {
					continue
				}
				descriptor, err := crane.Head(regexImg, images.WithGlobalInsecureFlag()...)
				if err != nil {
					// Test if this is a real image, if not just quiet log to debug, this is normal
					message.Debugf("Suspected image does not appear to be valid: %#v", err)
					l.Debug("suspected image does not appear to be valid", "error", err)
					continue
				}
				// Otherwise, add to the list of images
				message.Debugf("Imaged digest found: %s", descriptor.Digest)
				l.Debug("imaged digest found", "digest", descriptor.Digest)
				imgs = append(imgs, regexImg)
			}
			foundImages = append(foundImages, imgs...)
		}

		if !opt.SkipCosign {
			var cosignArtifactList []string
			for _, v := range foundImages {
				cosignArtifacts, err := utils.GetCosignArtifacts(v)
				if err != nil {
					return nil, fmt.Errorf("could not lookup the cosing artifacts for image %s: %w", v, err)
				}
				cosignArtifactList = append(cosignArtifactList, cosignArtifacts...)
			}
			foundImages = append(foundImages, cosignArtifactList...)
		}
		foundComponentImages[comp.Name] = foundImages
	}

	return foundComponentImages, nil
}

func getUnstructuredImages(resource *unstructured.Unstructured) ([]string, []string, error) {
	images := []string{}
	regexImages := []string{}

	contents := resource.UnstructuredContent()
	b, err := resource.MarshalJSON()
	if err != nil {
		return nil, nil, err
	}
	switch resource.GetKind() {
	case "Deployment":
		var deployment appsv1.Deployment
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(contents, &deployment)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse deployment: %w", err)
		}
		images = append(images, getImages(deployment.Spec.Template.Spec)...)
	case "DaemonSet":
		var daemonSet appsv1.DaemonSet
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(contents, &daemonSet)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse daemonset: %w", err)
		}
		images = append(images, getImages(daemonSet.Spec.Template.Spec)...)
	case "StatefulSet":
		var statefulSet appsv1.StatefulSet
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(contents, &statefulSet)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse statefulset: %w", err)
		}
		images = append(images, getImages(statefulSet.Spec.Template.Spec)...)
	case "ReplicaSet":
		var replicaSet appsv1.ReplicaSet
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(contents, &replicaSet)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse replicaset: %w", err)
		}
		images = append(images, getImages(replicaSet.Spec.Template.Spec)...)
	case "Job":
		var job batchv1.Job
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(contents, &job)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse job: %w", err)
		}
		images = append(images, getImages(job.Spec.Template.Spec)...)
	default:
		matches := imageCheck.FindAllStringSubmatch(string(b), -1)
		for _, group := range matches {
			regexImages = append(regexImages, group[1])
		}
	}

	// Capture maybe images too for all kinds because they might be in unexpected places.
	matches := imageFuzzyCheck.FindAllStringSubmatch(string(b), -1)
	for _, group := range matches {
		regexImages = append(regexImages, group[1])
	}

	return images, regexImages, nil
}

func getImages(pod corev1.PodSpec) []string {
	images := []string{}
	for _, container := range pod.InitContainers {
		images = append(images, container.Image)
	}
	for _, container := range pod.Containers {
		images = append(images, container.Image)
	}
	for _, container := range pod.EphemeralContainers {
		images = append(images, container.Image)
	}
	return images
}

func getSortedImages(matchedImages map[string]bool, maybeImages map[string]bool) ([]string, []string) {
	sortedMatchedImages := sort.StringSlice{}
	for image := range matchedImages {
		sortedMatchedImages = append(sortedMatchedImages, image)
	}
	sort.Sort(sortedMatchedImages)

	sortedMaybeImages := sort.StringSlice{}
	for image := range maybeImages {
		if matchedImages[image] {
			continue
		}
		sortedMaybeImages = append(sortedMaybeImages, image)
	}
	sort.Sort(sortedMaybeImages)

	return sortedMatchedImages, sortedMaybeImages
}
