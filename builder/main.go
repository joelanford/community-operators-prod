package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/operator-registry/alpha/action"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
	"github.com/operator-framework/operator-registry/pkg/image"
	"github.com/operator-framework/operator-registry/pkg/registry"
	"github.com/operator-framework/operator-registry/pkg/sqlite"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatalf("Usage: %s <rootDir> <dcDir> <ocpVersion>", os.Args[0])
	}
	if err := run(context.Background(), os.Args[1], os.Args[2], os.Args[3]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, inDir, outDir, targetOCPVersion string) error {
	if _, err := os.Stat(outDir); err == nil {
		return fmt.Errorf("output directory %q already exists", outDir)
	}
	packages, err := os.ReadDir(inDir)
	if err != nil {
		return err
	}

	nullLogger := log.New()
	nullLogger.Out = ioutil.Discard
	for _, p := range packages {
		if !p.IsDir() {
			continue
		}

		pkgDir := filepath.Join(inDir, p.Name())

		indexDir := filepath.Join(outDir, p.Name())
		indexFile := filepath.Join(indexDir, "index.yaml")
		pkgDirContents, err := os.ReadDir(pkgDir)
		if err != nil {
			return err
		}

		var cfg *declcfg.DeclarativeConfig
		if isPackageManifests(pkgDirContents) {
			cfg, err = renderPackageManifests(ctx, pkgDir)
		} else {
			cfg, err = renderBundles(ctx, pkgDir, pkgDirContents, targetOCPVersion)
		}
		if err != nil {
			log.Errorf("failed to render DC for package %q: %v", p.Name(), err)
			continue
		}

		if err := os.MkdirAll(indexDir, 0777); err != nil {
			return err
		}

		if err := func() error {
			f, err := os.OpenFile(indexFile, os.O_CREATE|os.O_WRONLY, 0666)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := declcfg.WriteYAML(*cfg, f); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return err
		}

		if !isPackageManifests(pkgDirContents) {
			mode := "semver-mode"
			ciFile, err := os.ReadFile(filepath.Join(pkgDir, "ci.yaml"))
			if err == nil {
				var ci struct {
					UpdateGraph string `json:"updateGraph"`
				}
				if err := yaml.Unmarshal(ciFile, &ci); err != nil {
					return err
				}
				mode = ci.UpdateGraph
			}

			var updateGraph *exec.Cmd
			switch mode {
			case "replaces-mode":
				updateGraph = exec.Command("declcfg", "inherit-channels", "-o", "yaml", indexDir, p.Name())
			case "semver-mode":
				updateGraph = exec.Command("declcfg", "semver", "-o", "yaml", indexDir, p.Name())
			case "semver-skippatch-mode":
				updateGraph = exec.Command("declcfg", "semver-skippatch", "-o", "yaml", indexDir, p.Name())
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

			inlineBundles := exec.Command("declcfg", "inline-bundles", "--prune", "-o", "yaml", indexDir, p.Name())
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
		}
	}
	return nil
}

func renderBundles(ctx context.Context, pkgDir string, pkgDirContents []os.DirEntry, targetOCPVersion string) (*declcfg.DeclarativeConfig, error) {
	packageName := filepath.Base(pkgDir)
	rbundles := []registry.Bundle{}
	versions := map[string]semver.Version{}
	for _, e := range pkgDirContents {
		if !e.IsDir() {
			continue
		}
		bundleImageRef := fmt.Sprintf("quay.io/openshift-community-operators/%s:v%s", packageName, e.Name())
		ii, err := registry.NewImageInput(image.SimpleReference(bundleImageRef), filepath.Join(pkgDir, e.Name()))
		if err != nil {
			return nil, err
		}

		b := ii.Bundle
		vStr, err := b.Version()
		if err != nil {
			return nil, err
		}
		v, err := semver.Parse(vStr)
		if err != nil {
			return nil, err
		}
		versions[b.Name] = v
		annotData, err := os.ReadFile(filepath.Join(pkgDir, vStr, "metadata", "annotations.yaml"))
		if err != nil {
			return nil, err
		}
		annots := struct {
			Annotations map[string]string `json:"annotations"`
		}{}
		if err := yaml.Unmarshal(annotData, &annots); err != nil {
			return nil, err
		}
		if r, ok := annots.Annotations["com.redhat.openshift.versions"]; ok {
			inRange, err := rangeContainsVersion(r, targetOCPVersion)
			if err != nil {
				return nil, err
			}
			if !inRange {
				continue
			}
		}

		rbundles = append(rbundles, *ii.Bundle)
	}

	cfg := &declcfg.DeclarativeConfig{}

	for _, b := range rbundles {
		dcb, err := bundleToDeclcfg(&b)
		if err != nil {
			return nil, err
		}
		cfg.Bundles = append(cfg.Bundles, dcb.Bundles...)
	}
	for _, b := range cfg.Bundles {
		props, err := property.Parse(b.Properties)
		if err != nil {
			return nil, err
		}
		if len(props.Packages) != 1 {
			return nil, fmt.Errorf("bundle %q must have exactly one olm.package property", b.Name)
		}

	}
	sort.Slice(rbundles, func(i, j int) bool {
		return versions[rbundles[i].Name].LT(versions[rbundles[j].Name])
	})

	defaultChannel := getDefaultChannel(rbundles)
	var (
		icon        *declcfg.Icon
		description string
	)
	for i := len(rbundles) - 1; i >= 0; i-- {
		b := rbundles[i]
		if sets.NewString(b.Channels...).Has(defaultChannel) {
			icons, err := b.Icons()
			if err != nil {
				return nil, err
			}
			if len(icons) > 0 {
				icon = &declcfg.Icon{
					Data:      icons[0].Base64data,
					MediaType: icons[0].MediaType,
				}
			}
			desc, err := b.Description()
			if err != nil {
				return nil, err
			}
			description = desc
			break
		}
	}
	cfg.Packages = []declcfg.Package{
		{
			Schema:         "olm.package",
			Name:           packageName,
			DefaultChannel: defaultChannel,
			Description:    description,
			Icon:           icon,
		},
	}
	return cfg, nil
}

func renderPackageManifests(ctx context.Context, ref string) (*declcfg.DeclarativeConfig, error) {
	tmpDB, err := os.CreateTemp("", "opm-render-pm-")
	if err != nil {
		return nil, err
	}
	if err := tmpDB.Close(); err != nil {
		return nil, err
	}

	db, err := sqlite.Open(tmpDB.Name())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	defer os.RemoveAll(tmpDB.Name())

	dbLoader, err := sqlite.NewSQLLiteLoader(db)
	if err != nil {
		return nil, err
	}
	if err := dbLoader.Migrate(context.TODO()); err != nil {
		return nil, err
	}

	loader := sqlite.NewSQLLoaderForDirectory(dbLoader, ref)
	if err := loader.Populate(); err != nil {
		return nil, fmt.Errorf("error loading manifests from directory: %s", err)
	}

	a := action.Render{
		Refs:           []string{tmpDB.Name()},
		AllowedRefMask: action.RefSqliteFile,
	}
	return a.Run(ctx)
}

func isPackageManifests(entries []os.DirEntry) bool {
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".package.yaml") || strings.HasSuffix(e.Name(), ".package.yml") {
			return true
		}
	}
	return false
}

func bundleToDeclcfg(bundle *registry.Bundle) (*declcfg.DeclarativeConfig, error) {
	bundleProperties, err := registry.PropertiesFromBundle(bundle)
	if err != nil {
		return nil, fmt.Errorf("get properties for bundle %q: %v", bundle.Name, err)
	}
	relatedImages, err := getRelatedImages(bundle)
	if err != nil {
		return nil, fmt.Errorf("get related images for bundle %q: %v", bundle.Name, err)
	}

	dBundle := declcfg.Bundle{
		Schema:        "olm.bundle",
		Name:          bundle.Name,
		Package:       bundle.Package,
		Image:         bundle.BundleImage,
		Properties:    bundleProperties,
		RelatedImages: relatedImages,
	}

	return &declcfg.DeclarativeConfig{Bundles: []declcfg.Bundle{dBundle}}, nil
}

func getRelatedImages(b *registry.Bundle) ([]declcfg.RelatedImage, error) {
	csv, err := b.ClusterServiceVersion()
	if err != nil {
		return nil, err
	}

	var objmap map[string]*json.RawMessage
	if err = json.Unmarshal(csv.Spec, &objmap); err != nil {
		return nil, err
	}

	rawValue, ok := objmap["relatedImages"]
	if !ok || rawValue == nil {
		return nil, err
	}

	var relatedImages []declcfg.RelatedImage
	if err = json.Unmarshal(*rawValue, &relatedImages); err != nil {
		return nil, err
	}
	return relatedImages, nil
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
		semverRangeStr := fmt.Sprintf(">=%s.0 <=%s.0", min, max)
		semverRange, err = semver.ParseRange(semverRangeStr)
		if err != nil {
			return false, fmt.Errorf("invalid range %q: %v", r, err)
		}
	default:
		return false, fmt.Errorf("invalid range %q", r)
	}
	return semverRange(compV), nil
}

func getDefaultChannel(sortedBundles []registry.Bundle) string {
	defaultChannel := ""
	for _, b := range sortedBundles {
		if b.Annotations == nil {
			continue
		}

		if defaultChannel == "" {
			defaultChannel = b.Annotations.SelectDefaultChannel()
		} else if b.Annotations.DefaultChannelName != "" {
			defaultChannel = b.Annotations.DefaultChannelName
		}
	}
	return defaultChannel
}
