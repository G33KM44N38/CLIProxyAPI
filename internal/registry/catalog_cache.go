package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
)

type catalogDiskCache struct{ directory string }

var modelCatalogDiskCache atomic.Pointer[catalogDiskCache]

// ConfigureModelCatalogCache selects a cache scoped to the server's auth directory.
// Configure it before starting either updater. Embedders can leave it disabled.
func ConfigureModelCatalogCache(directory string) {
	if directory == "" {
		modelCatalogDiskCache.Store(nil)
		return
	}
	modelCatalogDiskCache.Store(&catalogDiskCache{directory: directory})
}

func validateCachedCatalog(name string, data []byte) error {
	if len(data) > maxCodexClientModelsSize {
		return fmt.Errorf("catalog exceeds cache limit")
	}
	if name == "codex_client_models.json" {
		return ValidateCodexClientModelsJSON(data)
	}
	var parsed staticModelsJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	return validateModelsCatalog(&parsed)
}

func restoreCachedCatalog(name string) {
	cache := modelCatalogDiskCache.Load()
	if cache == nil {
		return
	}
	file, err := os.Open(filepath.Join(cache.directory, name))
	if err != nil {
		return
	}
	data, errRead := io.ReadAll(io.LimitReader(file, maxCodexClientModelsSize+1))
	errClose := file.Close()
	if errRead != nil || errClose != nil || validateCachedCatalog(name, data) != nil {
		log.Warn("Ignoring invalid model catalog cache; retaining embedded catalog")
		return
	}
	if name == "codex_client_models.json" {
		_, err = loadCodexClientModelsFromBytes(data, "disk cache")
	} else {
		err = loadModelsFromBytes(data, "disk cache")
	}
	if err != nil {
		log.Warn("Model catalog cache could not be loaded")
	}
}

func saveCachedCatalog(name string, data []byte) error {
	cache := modelCatalogDiskCache.Load()
	if cache == nil {
		return nil
	}
	if err := validateCachedCatalog(name, data); err != nil {
		return err
	}
	if err := os.MkdirAll(cache.directory, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(cache.directory, ".catalog-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if errRemove := os.Remove(temporary); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Debug("Could not remove temporary model catalog cache")
		}
	}()
	_, errWrite := file.Write(data)
	if errWrite == nil {
		errWrite = file.Sync()
	}
	errClose := file.Close()
	if errWrite != nil {
		return errWrite
	}
	if errClose != nil {
		return errClose
	}
	return os.Rename(temporary, filepath.Join(cache.directory, name))
}
