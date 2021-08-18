package pkg

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/operator-framework/api/pkg/manifests"
	"k8s.io/apimachinery/pkg/util/sets"
)

func LoadPackageManifest(path string) (*manifests.PackageManifest, *manifests.Bundle, map[string]manifests.Bundle, error) {
	pm, pkgBundles, err := manifests.GetManifestsDir(path)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(pm.Channels) == 0 {
		return nil, nil, nil, fmt.Errorf("package must specify at least one channel")
	}

	if pm.DefaultChannelName == "" {
		if len(pm.Channels) > 1 {
			return nil, nil, nil, fmt.Errorf("default channel must be specified if package contains multiple channels")
		}
		pm.DefaultChannelName = pm.Channels[0].Name
	}

	bundles := map[string]manifests.Bundle{}
	for _, b := range pkgBundles {
		bundles[b.Name] = *b
	}

	defChHeadName := ""
	for _, ch := range pm.Channels {
		if ch.Name == pm.DefaultChannelName {
			defChHeadName = ch.CurrentCSVName
		}
		curBundleName := ""
		toAdd := []string{ch.CurrentCSVName}
		for len(toAdd) > 0 {
			curBundleName, toAdd = toAdd[0], toAdd[1:]
			curBundle, ok := bundles[curBundleName]
			if !ok {
				return nil, nil, nil, fmt.Errorf("cannot find bundle %q in package", curBundleName)
			}
			curBundle.Channels = append(curBundle.Channels, ch.Name)
			bundles[curBundleName] = curBundle

			if curBundle.CSV.Spec.Replaces != "" {
				toAdd = append(toAdd, curBundle.CSV.Spec.Replaces)
			}
			toAdd = append(toAdd, curBundle.CSV.Spec.Skips...)
		}
	}
	if defChHeadName == "" {
		return nil, nil, nil, fmt.Errorf("default channel %q not found in channel list", pm.DefaultChannelName)
	}
	for name, bundle := range bundles {
		bundle.Channels = sets.NewString(bundle.Channels...).List()
		bundles[name] = bundle
	}

	defChHead, ok := bundles[defChHeadName]
	if !ok {
		return nil, nil, nil, fmt.Errorf("default channel head %q not found in package", defChHeadName)
	}

	return pm, &defChHead, bundles, nil
}

func LoadBundles(path string) ([]manifests.Bundle, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	bundles := []manifests.Bundle{}
	for _, e := range entries {
		bundleDir := filepath.Join(path, e.Name())
		b, err := manifests.GetBundleFromDir(bundleDir)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, *b)
	}
	SortBundles(bundles)
	return bundles, nil
}

func SortBundles(bundles []manifests.Bundle) {
	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].CSV.Spec.Version.Version.LT(bundles[j].CSV.Spec.Version.Version)
	})
}
