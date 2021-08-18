package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/operator-registry/alpha/action"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/pkg/image"
	"github.com/operator-framework/operator-registry/pkg/registry"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

type config struct {
	IndexImageRepo      string   `json:"indexImageRepo"`
	DefaultMode         string   `json:"defaultMode"`
	BundleImageTemplate string   `json:"bundleImageTemplate"`
	OpenShiftVersions   []string `json:"openshiftVersions"`

	bundleImgTmpl template.Template
}

type pkg struct {
	name           string
	updateMode     string
	defaultChannel string
	icon           *declcfg.Icon
	description    string
	bundles        []bundle
}
type bundle struct {
	packageName          string
	image                string
	supportedOCPVersions sets.String
	version              semver.Version
	blob                 declcfg.Bundle

	// only used when parsing bundle format (not packagemanifests)
	rbundle *registry.Bundle
}

func main() {
	logrus.SetOutput(ioutil.Discard)
	log := logrus.New()
	if len(os.Args) != 4 {
		log.Fatalf("Usage: %s <rootDir> <outputDir> <configFile>", os.Args[0])
	}
	if err := run(context.Background(), log, os.Args[1], os.Args[2], os.Args[3]); err != nil {
		log.Fatal(err)
	}
}

func readConfig(path string) (*config, error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := yaml.Unmarshal(d, &cfg); err != nil {
		return nil, err
	}
	tmpl, err := template.New("bundleImage").Parse(cfg.BundleImageTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse bundle image template %q: %v", cfg.BundleImageTemplate, err)
	}
	cfg.bundleImgTmpl = *tmpl
	return &cfg, nil
}

func run(ctx context.Context, log *logrus.Logger, inDir, outDir, configFile string) error {
	if _, err := os.Stat(outDir); err == nil {
		return fmt.Errorf("output directory %q already exists", outDir)
	}

	cfg, err := readConfig(configFile)
	if err != nil {
		return err
	}

	pkgsByVersion, err := loadPackages(ctx, log, *cfg, inDir)
	if err != nil {
		return fmt.Errorf("load packages from %s: %v", inDir, err)
	}

	for ocpVer, pkgs := range pkgsByVersion {
		indexDir := filepath.Join(outDir, ocpVer)
		for _, p := range pkgs {
			cfg := declcfg.DeclarativeConfig{}
			cfg.Packages = []declcfg.Package{
				{
					Schema:         "olm.package",
					Name:           p.name,
					DefaultChannel: p.defaultChannel,
					Icon:           p.icon,
					Description:    p.description,
				},
			}
			for _, b := range p.bundles {
				cfg.Bundles = append(cfg.Bundles, b.blob)
			}

			indexFile := filepath.Join(indexDir, p.name, "index.yaml")
			if err := os.MkdirAll(filepath.Dir(indexFile), 0777); err != nil {
				return err
			}
			buf := bytes.Buffer{}
			if err := declcfg.WriteYAML(cfg, &buf); err != nil {
				return err
			}
			if err := os.WriteFile(indexFile, buf.Bytes(), 0666); err != nil {
				return err
			}

			var updateGraph *exec.Cmd
			switch p.updateMode {
			case "replaces-mode":
				updateGraph = exec.Command("declcfg", "inherit-channels", "-o", "yaml", indexDir, p.name)
			case "semver-mode":
				updateGraph = exec.Command("declcfg", "semver", "-o", "yaml", indexDir, p.name)
			case "semver-skippatch-mode":
				updateGraph = exec.Command("declcfg", "semver-skippatch", "-o", "yaml", indexDir, p.name)
			}
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			updateGraph.Stdout = stdout
			updateGraph.Stderr = stderr
			if err := updateGraph.Run(); err != nil {
				return errors.New(stderr.String())
			}
			if err := os.WriteFile(indexFile, stdout.Bytes(), 0666); err != nil {
				return err
			}

			inlineBundles := exec.Command("declcfg", "inline-bundles", "--prune", "-o", "yaml", indexDir, p.name)
			stdout = &bytes.Buffer{}
			stderr = &bytes.Buffer{}
			inlineBundles.Stdout = stdout
			inlineBundles.Stderr = stderr
			if err := inlineBundles.Run(); err != nil {
				return errors.New(stderr.String())
			}
			if err := os.WriteFile(indexFile, stdout.Bytes(), 0666); err != nil {
				return err
			}
			log.Infof("wrote %q", indexFile)
		}
	}

	return nil
}

type result struct {
	pkgDir string
	p      map[string]pkg
	err    error
}

func worker(ctx context.Context, log *logrus.Logger, cfg config, pkgDirs <-chan string, results chan<- result) {
	for pkgDir := range pkgDirs {
		log.Infof("loading package directory %q", filepath.Base(pkgDir))
		p, err := loadPackage(ctx, log, cfg, pkgDir)
		results <- result{pkgDir, p, err}
	}
}

func loadPackages(ctx context.Context, log *logrus.Logger, cfg config, inDir string) (map[string][]pkg, error) {
	pkgs := map[string][]pkg{}

	inDirEntries, err := os.ReadDir(inDir)
	if err != nil {
		return nil, err
	}

	pkgDirs := []string{}
	for _, e := range inDirEntries {
		if !e.IsDir() {
			continue
		}
		pkgDirs = append(pkgDirs, filepath.Join(inDir, e.Name()))
	}

	pkgDirCh := make(chan string, len(pkgDirs))
	resultCh := make(chan result, len(pkgDirs))

	for w := 0; w < runtime.NumCPU()*2; w++ {
		go worker(ctx, log, cfg, pkgDirCh, resultCh)
	}

	for _, e := range inDirEntries {
		if !e.IsDir() {
			continue
		}
		pkgDir := filepath.Join(inDir, e.Name())
		pkgDirCh <- pkgDir
	}
	close(pkgDirCh)

	for i := 0; i < len(pkgDirs); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-resultCh:
			pkgDir, p, err := r.pkgDir, r.p, r.err
			if err != nil {
				log.Errorf("load package %s: %v", filepath.Base(pkgDir), err)
				continue
			}
			for ocpVer, vp := range p {
				pkgs[ocpVer] = append(pkgs[ocpVer], vp)
			}
		}
	}
	return pkgs, nil
}

func loadPackage(ctx context.Context, log *logrus.Logger, cfg config, pkgDir string) (map[string]pkg, error) {
	ciYamlFile := filepath.Join(pkgDir, "ci.yaml")
	updateMode, err := readCIYamlMode(ciYamlFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("load ci.yaml: %v", err)
		}
		updateMode = cfg.DefaultMode
	}

	pkgDirEntries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, err
	}
	var p map[string]pkg
	if pkgYAML := getPackageYAML(pkgDirEntries); pkgYAML != "" {
		p, err = loadPackageManifest(ctx, log, cfg, pkgDir, pkgYAML, pkgDirEntries)
	} else {
		p, err = loadBundles(ctx, cfg, pkgDir, pkgDirEntries)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range p {
		v.updateMode = updateMode
		p[k] = v
	}

	return p, nil
}

func getPackageYAML(entries []os.DirEntry) string {
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".package.yaml") || strings.HasSuffix(e.Name(), ".package.yml") {
			return e.Name()
		}
	}
	return ""
}

func loadPackageManifest(ctx context.Context, log *logrus.Logger, cfg config, pkgDir string, pkgYAML string, pkgDirEntries []os.DirEntry) (map[string]pkg, error) {
	var pm registry.PackageManifest
	d, err := os.ReadFile(filepath.Join(pkgDir, pkgYAML))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(d, &pm); err != nil {
		return nil, err
	}

	var (
		eg      errgroup.Group
		mu      sync.Mutex
		bundles []bundle
	)
	csvs := map[string]registry.ClusterServiceVersion{}
	for _, e := range pkgDirEntries {
		e := e
		if !e.IsDir() {
			continue
		}
		eg.Go(func() error {
			v, err := semver.Parse(e.Name())
			if err != nil {
				return fmt.Errorf("directory %q not valid semver: %v", e.Name(), err)
			}
			t := struct {
				PackageName string
				Version     string
			}{
				PackageName: pm.PackageName,
				Version:     v.String(),
			}
			buf := &bytes.Buffer{}
			if err := cfg.bundleImgTmpl.Execute(buf, t); err != nil {
				return err
			}
			img := buf.String()
			csv, err := registry.ReadCSVFromBundleDirectory(filepath.Join(pkgDir, e.Name()))
			if err != nil {
				return err
			}
			r := action.Render{
				Refs:           []string{img},
				AllowedRefMask: action.RefBundleImage,
			}

			isRetriable := func(err error) bool { return true }

			var bcfg *declcfg.DeclarativeConfig
			renderBundle := func() error {
				bcfg, err = r.Run(ctx)
				return err
			}

			if err := retry.OnError(retry.DefaultBackoff, isRetriable, renderBundle); err != nil {
				return fmt.Errorf("render bundle %q: %v", img, err)
			}

			mu.Lock()
			defer mu.Unlock()
			csvs[csv.Name] = *csv
			bundles = append(bundles, bundle{
				packageName:          pm.PackageName,
				version:              v,
				image:                img,
				supportedOCPVersions: sets.NewString(cfg.OpenShiftVersions...),
				blob:                 bcfg.Bundles[0],
			})
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].version.LT(bundles[j].version)
	})

	defaultChannelName := pm.DefaultChannelName
	if defaultChannelName == "" {
		if len(pm.Channels) != 1 {
			return nil, fmt.Errorf("default channel must be defined when multiple channels are defined")
		}
		defaultChannelName = pm.Channels[0].Name
	}

	defaultChannelHead := ""
	for _, ch := range pm.Channels {
		if ch.Name == defaultChannelName {
			defaultChannelHead = ch.CurrentCSVName
			break
		}
	}

	var icon *declcfg.Icon
	var description string
	if defaultChannelHead != "" {
		defHead, ok := csvs[defaultChannelHead]
		if !ok {
			return nil, fmt.Errorf("csv for default channel head %q not found", defaultChannelHead)
		}
		icons, err := defHead.GetIcons()
		if err != nil {
			log.Warnf("ignored icons for default channel head %q: %v", defaultChannelHead, err)
		}
		if len(icons) > 0 {
			icon = &declcfg.Icon{
				Data:      icons[0].Base64data,
				MediaType: icons[0].MediaType,
			}
		}
		description, err = defHead.GetDescription()
		if err != nil {
			return nil, fmt.Errorf("get description for default channel head %q: %v", defaultChannelHead, err)
		}
	}

	p := pkg{
		name:           pm.PackageName,
		defaultChannel: defaultChannelName,
		icon:           icon,
		description:    description,
		bundles:        bundles,
	}
	pkgs := map[string]pkg{}
	for _, ocpVer := range cfg.OpenShiftVersions {
		pkgs[ocpVer] = p
	}

	return pkgs, nil
}

func loadBundles(ctx context.Context, cfg config, pkgDir string, pkgDirEntries []os.DirEntry) (map[string]pkg, error) {
	pkgName := filepath.Base(pkgDir)

	var (
		eg         errgroup.Group
		mu         sync.Mutex
		allBundles []bundle
	)
	for _, e := range pkgDirEntries {
		e := e
		if !e.IsDir() {
			continue
		}
		eg.Go(func() error {
			v, err := semver.Parse(e.Name())
			if err != nil {
				return fmt.Errorf("directory %q not valid semver: %v", e.Name(), err)
			}
			t := struct {
				PackageName string
				Version     string
			}{
				PackageName: pkgName,
				Version:     v.String(),
			}
			buf := &bytes.Buffer{}
			if err := cfg.bundleImgTmpl.Execute(buf, t); err != nil {
				return err
			}
			img := buf.String()
			annotData, err := os.ReadFile(filepath.Join(pkgDir, v.String(), "metadata", "annotations.yaml"))
			if err != nil {
				return err
			}
			annots := struct {
				Annotations map[string]string `json:"annotations"`
			}{}
			if err := yaml.Unmarshal(annotData, &annots); err != nil {
				return err
			}

			rbundle, err := registry.NewImageInput(image.SimpleReference(img), filepath.Join(pkgDir, v.String()))
			if err != nil {
				return fmt.Errorf("parse bundle %q: %v", v.String(), err)
			}
			if rbundle.Bundle.Annotations.PackageName != pkgName {
				return fmt.Errorf("package name mismatch: directory name is %q, bundle %q package name annotation is %q", pkgName, e.Name(), rbundle.Bundle.Annotations.PackageName)
			}

			supportedOCPVersions := sets.NewString()

			versionRange, ok := annots.Annotations["com.redhat.openshift.versions"]
			if !ok {
				supportedOCPVersions = supportedOCPVersions.Insert(cfg.OpenShiftVersions...)
			} else {
				for _, ocpVer := range cfg.OpenShiftVersions {
					inRange, err := rangeContainsVersion(versionRange, ocpVer)
					if err != nil {
						return err
					}
					if inRange {
						supportedOCPVersions = supportedOCPVersions.Insert(ocpVer)
					}
				}
			}

			r := action.Render{
				Refs:           []string{img},
				AllowedRefMask: action.RefBundleImage,
			}
			isRetriable := func(err error) bool { return true }

			var bcfg *declcfg.DeclarativeConfig
			renderBundle := func() error {
				bcfg, err = r.Run(ctx)
				return err
			}

			if err := retry.OnError(retry.DefaultBackoff, isRetriable, renderBundle); err != nil {
				return fmt.Errorf("render bundle %q: %v", img, err)
			}

			mu.Lock()
			defer mu.Unlock()
			allBundles = append(allBundles, bundle{
				packageName:          pkgName,
				version:              v,
				image:                img,
				supportedOCPVersions: supportedOCPVersions,
				blob:                 bcfg.Bundles[0],
				rbundle:              rbundle.Bundle,
			})
			return nil
		})

	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	pkgs := map[string]pkg{}
	for _, ocpVer := range cfg.OpenShiftVersions {
		bundles := []bundle{}
		for _, b := range allBundles {
			if b.supportedOCPVersions.Has(ocpVer) {
				bundles = append(bundles, b)
			}
		}

		sort.Slice(bundles, func(i, j int) bool {
			return bundles[i].version.LT(bundles[j].version)
		})
		defaultChannel := getDefaultChannel(bundles)

		var (
			icon        *declcfg.Icon
			description string
		)
		for i := len(bundles) - 1; i >= 0; i-- {
			b := bundles[i]
			if sets.NewString(b.rbundle.Channels...).Has(defaultChannel) {
				icons, err := b.rbundle.Icons()
				if err != nil {
					return nil, err
				}
				if len(icons) > 0 {
					icon = &declcfg.Icon{
						Data:      icons[0].Base64data,
						MediaType: icons[0].MediaType,
					}
				}
				desc, err := b.rbundle.Description()
				if err != nil {
					return nil, err
				}
				description = desc
				break
			}
		}

		if len(bundles) > 0 {
			pkgs[ocpVer] = pkg{
				name:           pkgName,
				defaultChannel: defaultChannel,
				icon:           icon,
				description:    description,
				bundles:        bundles,
			}
		}
	}
	return pkgs, nil
}

func readCIYamlMode(path string) (string, error) {
	ciFile, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var ci struct {
		UpdateGraph string `json:"updateGraph"`
	}
	if err := yaml.Unmarshal(ciFile, &ci); err != nil {
		return "", err
	}
	return ci.UpdateGraph, nil
}

var ocpVerRegex = regexp.MustCompile(`^v\d+\.\d+`)

func rangeContainsVersion(r string, v string) (bool, error) {
	if len(r) == 0 {
		return false, errors.New("range is empty")
	}
	if len(v) == 0 {
		return false, errors.New("version is empty")
	}

	v = strings.TrimPrefix(v, "v")
	compV, err := semver.Parse(v + ".0")
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %v", v, err)
	}

	// special legacy cases
	if r == "v4.5,v4.6" || r == "v4.6,v4.5" {
		semverRange := semver.MustParseRange(">=4.5.0")
		return semverRange(compV), nil
	}

	var semverRange semver.Range
	rs := strings.SplitN(r, "-", 2)
	switch len(rs) {
	case 1:
		// Range specify exact version
		if strings.HasPrefix(r, "=") {
			trimmed := strings.TrimPrefix(r, "=v")
			semverRange, err = semver.ParseRange(fmt.Sprintf("%s.0", trimmed))
		} else {
			trimmed := strings.TrimPrefix(r, "v")
			// Range specifies minimum version
			semverRange, err = semver.ParseRange(fmt.Sprintf(">=%s.0", trimmed))
		}
		if err != nil {
			return false, fmt.Errorf("invalid range %q: %v", r, err)
		}
	case 2:
		min := rs[0]
		max := rs[1]
		if strings.HasPrefix(min, "=") || strings.HasPrefix(max, "=") {
			return false, fmt.Errorf("invalid range %q: cannot use equal prefix with range", r)
		}
		trimmedMin := strings.TrimPrefix(min, "v")
		trimmedMax := strings.TrimPrefix(max, "v")

		semverRangeStr := fmt.Sprintf(">=%s.0 <=%s.0", trimmedMin, trimmedMax)
		semverRange, err = semver.ParseRange(semverRangeStr)
		if err != nil {
			return false, fmt.Errorf("invalid range %q: %v", r, err)
		}
	default:
		return false, fmt.Errorf("invalid range %q", r)
	}
	return semverRange(compV), nil
}

func getDefaultChannel(bundles []bundle) string {
	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].version.LT(bundles[j].version)
	})
	defaultChannel := ""
	for _, b := range bundles {
		if b.rbundle.Annotations == nil {
			continue
		}
		if defaultChannel == "" {
			defaultChannel = b.rbundle.Annotations.SelectDefaultChannel()
		} else if b.rbundle.Annotations.DefaultChannelName != "" {
			defaultChannel = b.rbundle.Annotations.DefaultChannelName
		}
	}
	return defaultChannel
}

//func foobar() error {
//	var cfg config
//	var inDir string
//
//	bundleImageTmpl, err := template.New("bundleImage").Parse(cfg.BundleImageTemplate)
//	if err != nil {
//		return err
//	}
//
//	nullLogger := log.New()
//	nullLogger.Out = ioutil.Discard
//	for _, p := range packages {
//		if !p.IsDir() {
//			continue
//		}
//
//		pkgDir := filepath.Join(inDir, p.Name())
//		ciYamlFile := filepath.Join(pkgDir, "ci.yaml")
//		ciMode, err := readCIYamlMode(ciYamlFile)
//		if err != nil {
//			if !os.IsNotExist(err) {
//				return err
//			}
//			ciMode = cfg.DefaultMode
//		}
//
//		pkgDirContents, err := os.ReadDir(pkgDir)
//		if err != nil {
//			return err
//		}
//		bundles := []bundle{}
//		for _, b := range pkgDirContents {
//			if !b.IsDir() {
//				continue
//			}
//			v, err := semver.Parse(b.Name())
//			if err != nil {
//				continue
//			}
//			t := struct {
//				PackageName string
//				Version     string
//			}{
//				PackageName: p.Name(),
//				Version:     v.String(),
//			}
//			buf := &bytes.Buffer{}
//			if err := bundleImageTmpl.Execute(buf, t); err != nil {
//				return err
//			}
//			image := buf.String()
//			var supportedOCPVersions []string
//			if isPackageManifests(pkgDirContents) {
//				supportedOCPVersions = cfg.OpenShiftVersions
//			} else {
//				annotData, err := os.ReadFile(filepath.Join(pkgDir, v.String(), "metadata", "annotations.yaml"))
//				if err != nil {
//					return err
//				}
//				annots := struct {
//					Annotations map[string]string `json:"annotations"`
//				}{}
//				if err := yaml.Unmarshal(annotData, &annots); err != nil {
//					return err
//				}
//
//				r, ok := annots.Annotations["com.redhat.openshift.versions"]
//				for _, ocpVer := range cfg.OpenShiftVersions {
//					if !ok {
//						supportedOCPVersions = append(supportedOCPVersions, ocpVer)
//						continue
//					}
//					inRange, err := rangeContainsVersion(r, ocpVer)
//					if err != nil {
//						return err
//					}
//					if inRange {
//						supportedOCPVersions = append(supportedOCPVersions, ocpVer)
//					}
//				}
//			}
//			bundles = append(bundles, bundle{
//				image:                image,
//				supportedOCPVersions: sets.NewString(supportedOCPVersions...),
//				version:              v,
//			})
//		}
//
//		sort.Slice(bundles, func(i, j int) bool {
//			return bundles[i].version.LT(bundles[j].version)
//		})
//
//		for _, ocpVer := range cfg.OpenShiftVersions {
//			idx := index{
//				ocpVersion: ocpVer,
//				packages:   map[string]pkg{},
//			}
//			_ = idx
//		}

//indexDir := filepath.Join(outDir, p.Name())
//indexFile := filepath.Join(indexDir, "index.yaml")
//pkgDirContents, err := os.ReadDir(pkgDir)
//if err != nil {
//	return err
//}

//var cfg *declcfg.DeclarativeConfig
//if isPackageManifests(pkgDirContents) {
//	cfg, err = renderPackageManifests(ctx, pkgDir)
//} else {
//	targetOCPVersion := "4.8"
//	cfg, err = renderBundles(ctx, pkgDir, pkgDirContents, targetOCPVersion)
//}
//if err != nil {
//	log.Errorf("failed to render DC for package %q: %v", p.Name(), err)
//	continue
//}
//
//if err := os.MkdirAll(indexDir, 0777); err != nil {
//	return err
//}
//
//if err := func() error {
//	f, err := os.OpenFile(indexFile, os.O_CREATE|os.O_WRONLY, 0666)
//	if err != nil {
//		return err
//	}
//	defer f.Close()
//	if err := declcfg.WriteYAML(*cfg, f); err != nil {
//		return err
//	}
//	return nil
//}(); err != nil {
//	return err
//}
//
//if !isPackageManifests(pkgDirContents) {
//	mode := "semver-mode"
//	ciFile, err := os.ReadFile(filepath.Join(pkgDir, "ci.yaml"))
//	if err == nil {
//		var ci struct {
//			UpdateGraph string `json:"updateGraph"`
//		}
//		if err := yaml.Unmarshal(ciFile, &ci); err != nil {
//			return err
//		}
//		mode = ci.UpdateGraph
//	}
//
//	var updateGraph *exec.Cmd
//	switch mode {
//	case "replaces-mode":
//		updateGraph = exec.Command("declcfg", "inherit-channels", "-o", "yaml", indexDir, p.Name())
//	case "semver-mode":
//		updateGraph = exec.Command("declcfg", "semver", "-o", "yaml", indexDir, p.Name())
//	case "semver-skippatch-mode":
//		updateGraph = exec.Command("declcfg", "semver-skippatch", "-o", "yaml", indexDir, p.Name())
//	}
//	stdout := &bytes.Buffer{}
//	stderr := &bytes.Buffer{}
//	updateGraph.Stdout = stdout
//	updateGraph.Stderr = stderr
//	if err := updateGraph.Run(); err != nil {
//		return errors.New(stderr.String())
//	}
//	if err := os.WriteFile(indexFile, stdout.Bytes(), 0666); err != nil {
//		return err
//	}
//
//	inlineBundles := exec.Command("declcfg", "inline-bundles", "--prune", "-o", "yaml", indexDir, p.Name())
//	stdout = &bytes.Buffer{}
//	stderr = &bytes.Buffer{}
//	inlineBundles.Stdout = stdout
//	inlineBundles.Stderr = stderr
//	if err := inlineBundles.Run(); err != nil {
//		return errors.New(stderr.String())
//	}
//	if err := os.WriteFile(indexFile, stdout.Bytes(), 0666); err != nil {
//		return err
//	}
//}
//	}
//	return nil
//}

//func renderBundles(ctx context.Context, pkgDir string, pkgDirContents []os.DirEntry, targetOCPVersion string) (*declcfg.DeclarativeConfig, error) {
//	packageName := filepath.Base(pkgDir)
//	rbundles := []registry.Bundle{}
//	versions := map[string]semver.Version{}
//	for _, e := range pkgDirContents {
//		if !e.IsDir() {
//			continue
//		}
//		bundleImageRef := fmt.Sprintf("quay.io/openshift-community-operators/%s:v%s", packageName, e.Name())
//		ii, err := registry.NewImageInput(image.SimpleReference(bundleImageRef), filepath.Join(pkgDir, e.Name()))
//		if err != nil {
//			return nil, err
//		}
//
//		b := ii.Bundle
//		vStr, err := b.Version()
//		if err != nil {
//			return nil, err
//		}
//		v, err := semver.Parse(vStr)
//		if err != nil {
//			return nil, err
//		}
//		versions[b.Name] = v
//		annotData, err := os.ReadFile(filepath.Join(pkgDir, vStr, "metadata", "annotations.yaml"))
//		if err != nil {
//			return nil, err
//		}
//		annots := struct {
//			Annotations map[string]string `json:"annotations"`
//		}{}
//		if err := yaml.Unmarshal(annotData, &annots); err != nil {
//			return nil, err
//		}
//		if r, ok := annots.Annotations["com.redhat.openshift.versions"]; ok {
//			inRange, err := rangeContainsVersion(r, targetOCPVersion)
//			if err != nil {
//				return nil, err
//			}
//			if !inRange {
//				continue
//			}
//		}
//
//		rbundles = append(rbundles, *ii.Bundle)
//	}
//
//	cfg := &declcfg.DeclarativeConfig{}
//
//	for _, b := range rbundles {
//		dcb, err := bundleToDeclcfg(&b)
//		if err != nil {
//			return nil, err
//		}
//		cfg.Bundles = append(cfg.Bundles, dcb.Bundles...)
//	}
//	for _, b := range cfg.Bundles {
//		props, err := property.Parse(b.Properties)
//		if err != nil {
//			return nil, err
//		}
//		if len(props.Packages) != 1 {
//			return nil, fmt.Errorf("bundle %q must have exactly one olm.package property", b.Name)
//		}
//
//	}
//	sort.Slice(rbundles, func(i, j int) bool {
//		return versions[rbundles[i].Name].LT(versions[rbundles[j].Name])
//	})
//
//	defaultChannel := getDefaultChannel(rbundles)
//	var (
//		icon        *declcfg.Icon
//		description string
//	)
//	for i := len(rbundles) - 1; i >= 0; i-- {
//		b := rbundles[i]
//		if sets.NewString(b.Channels...).Has(defaultChannel) {
//			icons, err := b.Icons()
//			if err != nil {
//				return nil, err
//			}
//			if len(icons) > 0 {
//				icon = &declcfg.Icon{
//					Data:      icons[0].Base64data,
//					MediaType: icons[0].MediaType,
//				}
//			}
//			desc, err := b.Description()
//			if err != nil {
//				return nil, err
//			}
//			description = desc
//			break
//		}
//	}
//	cfg.Packages = []declcfg.Package{
//		{
//			Schema:         "olm.package",
//			Name:           packageName,
//			DefaultChannel: defaultChannel,
//			Description:    description,
//			Icon:           icon,
//		},
//	}
//	return cfg, nil
//}

//
//func renderPackageManifests(ctx context.Context, ref string) (*declcfg.DeclarativeConfig, error) {
//	tmpDB, err := os.CreateTemp("", "opm-render-pm-")
//	if err != nil {
//		return nil, err
//	}
//	if err := tmpDB.Close(); err != nil {
//		return nil, err
//	}
//
//	db, err := sqlite.Open(tmpDB.Name())
//	if err != nil {
//		return nil, err
//	}
//	defer db.Close()
//	defer os.RemoveAll(tmpDB.Name())
//
//	dbLoader, err := sqlite.NewSQLLiteLoader(db)
//	if err != nil {
//		return nil, err
//	}
//	if err := dbLoader.Migrate(context.TODO()); err != nil {
//		return nil, err
//	}
//
//	loader := sqlite.NewSQLLoaderForDirectory(dbLoader, ref)
//	if err := loader.Populate(); err != nil {
//		return nil, fmt.Errorf("error loading manifests from directory: %s", err)
//	}
//
//	a := action.Render{
//		Refs:           []string{tmpDB.Name()},
//		AllowedRefMask: action.RefSqliteFile,
//	}
//	return a.Run(ctx)
//}

//func isPackageManifests(entries []os.DirEntry) bool {
//	for _, e := range entries {
//		if strings.HasSuffix(e.Name(), ".package.yaml") || strings.HasSuffix(e.Name(), ".package.yml") {
//			return true
//		}
//	}
//	return false
//}

//func bundleToDeclcfg(bundle *registry.Bundle) (*declcfg.DeclarativeConfig, error) {
//	bundleProperties, err := registry.PropertiesFromBundle(bundle)
//	if err != nil {
//		return nil, fmt.Errorf("get properties for bundle %q: %v", bundle.Name, err)
//	}
//	relatedImages, err := getRelatedImages(bundle)
//	if err != nil {
//		return nil, fmt.Errorf("get related images for bundle %q: %v", bundle.Name, err)
//	}
//
//	dBundle := declcfg.Bundle{
//		Schema:        "olm.bundle",
//		Name:          bundle.Name,
//		Package:       bundle.Package,
//		Image:         bundle.BundleImage,
//		Properties:    bundleProperties,
//		RelatedImages: relatedImages,
//	}
//
//	return &declcfg.DeclarativeConfig{Bundles: []declcfg.Bundle{dBundle}}, nil
//}
//
//func getRelatedImages(b *registry.Bundle) ([]declcfg.RelatedImage, error) {
//	csv, err := b.ClusterServiceVersion()
//	if err != nil {
//		return nil, err
//	}
//
//	var objmap map[string]*json.RawMessage
//	if err = json.Unmarshal(csv.Spec, &objmap); err != nil {
//		return nil, err
//	}
//
//	rawValue, ok := objmap["relatedImages"]
//	if !ok || rawValue == nil {
//		return nil, err
//	}
//
//	var relatedImages []declcfg.RelatedImage
//	if err = json.Unmarshal(*rawValue, &relatedImages); err != nil {
//		return nil, err
//	}
//	return relatedImages, nil
//}
