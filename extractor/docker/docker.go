package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"

	"github.com/knqyf263/fanal/extractor"
	"github.com/knqyf263/fanal/extractor/docker/token/ecr"
	"github.com/knqyf263/fanal/extractor/docker/token/gcr"
	"github.com/knqyf263/fanal/types"

	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/docker/client"
	"github.com/genuinetools/reg/registry"
	"github.com/knqyf263/fanal/cache"
	"github.com/knqyf263/nested"
	"golang.org/x/xerrors"
)

const (
	opq string = ".wh..wh..opq"
	wh  string = ".wh."
)

type manifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type Config struct {
	ContainerConfig containerConfig `json:"container_config"`
	History         []History
}

type containerConfig struct {
	Env []string
}

type History struct {
	Created   time.Time
	CreatedBy string `json:"created_by"`
}

type layer struct {
	ID      digest.Digest
	Content io.ReadCloser
}

type opqDirs []string

type DockerExtractor struct {
	Option types.DockerOption
}

func NewDockerExtractor(option types.DockerOption) DockerExtractor {
	RegisterRegistry(&gcr.GCR{})
	RegisterRegistry(&ecr.ECR{})
	return DockerExtractor{Option: option}
}

func applyLayers(layerPaths []string, filesInLayers map[string]extractor.FileMap, opqInLayers map[string]opqDirs) (extractor.FileMap, error) {
	sep := "/"
	nestedMap := nested.Nested{}
	for _, layerPath := range layerPaths {
		for _, opqDir := range opqInLayers[layerPath] {
			nestedMap.DeleteByString(opqDir, sep)
		}

		for filePath, content := range filesInLayers[layerPath] {
			fileName := filepath.Base(filePath)
			fileDir := filepath.Dir(filePath)
			switch {
			case strings.HasPrefix(fileName, wh):
				fname := strings.TrimPrefix(fileName, wh)
				fpath := filepath.Join(fileDir, fname)
				nestedMap.DeleteByString(fpath, sep)
			default:
				nestedMap.SetByString(filePath, sep, content)
			}
		}
	}

	fileMap := extractor.FileMap{}
	walkFn := func(keys []string, value interface{}) error {
		content, ok := value.([]byte)
		if !ok {
			return nil
		}
		path := strings.Join(keys, "/")
		fileMap[path] = content
		return nil
	}
	if err := nestedMap.Walk(walkFn); err != nil {
		return nil, xerrors.Errorf("failed to walk nested map: %w", err)
	}

	return fileMap, nil

}

func (d DockerExtractor) createRegistryClient(ctx context.Context, domain string) (*registry.Registry, error) {
	auth, err := GetToken(ctx, domain, d.Option)
	if err != nil {
		return nil, xerrors.Errorf("failed to get auth config: %w", err)
	}

	// Prevent non-ssl unless explicitly forced
	if !d.Option.NonSSL && strings.HasPrefix(auth.ServerAddress, "http:") {
		return nil, xerrors.New("attempted to use insecure protocol! Use force-non-ssl option to force")
	}

	// Create the registry client.
	return registry.New(ctx, auth, registry.Opt{
		Domain:   domain,
		Insecure: d.Option.Insecure,
		Debug:    d.Option.Debug,
		SkipPing: d.Option.SkipPing,
		NonSSL:   d.Option.NonSSL,
		Timeout:  d.Option.Timeout,
	})
}

func (d DockerExtractor) SaveLocalImage(ctx context.Context, imageName string) (io.Reader, error) {
	var err error
	r := cache.Get(imageName)
	if r == nil {
		// Save the image
		r, err = d.saveLocalImage(ctx, imageName)
		if err != nil {
			return nil, err
		}
		r, err = cache.Set(imageName, r)
		if err != nil {
			log.Print(err)
		}
	}

	return r, nil
}

func (d DockerExtractor) saveLocalImage(ctx context.Context, imageName string) (io.ReadCloser, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, xerrors.New("error in docker NewClient")
	}

	r, err := cli.ImageSave(ctx, []string{imageName})
	if err != nil {
		return nil, xerrors.New("error in docker image save")
	}
	return r, nil
}

func (d DockerExtractor) Extract(ctx context.Context, imageName string, filenames []string) (extractor.FileMap, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.Option.Timeout)
	defer cancel()

	image, err := registry.ParseImage(imageName)
	if err != nil {
		return nil, err
	}
	r, err := d.createRegistryClient(ctx, image.Domain)
	if err != nil {
		return nil, err
	}

	// Get the v2 manifest.
	manifest, err := r.Manifest(ctx, image.Path, image.Reference())
	if err != nil {
		return nil, err
	}
	m, ok := manifest.(*schema2.DeserializedManifest)
	if !ok {
		return nil, xerrors.New("invalid manifest")
	}

	ch := make(chan layer)
	errCh := make(chan error)
	layerIDs := []string{}
	for _, ref := range m.Manifest.Layers {
		layerIDs = append(layerIDs, string(ref.Digest))
		go func(d digest.Digest) {
			// Use cache
			rc := cache.Get(string(d))
			if rc == nil {
				// Download the layer.
				rc, err = r.DownloadLayer(ctx, image.Path, d)
				if err != nil {
					errCh <- xerrors.Errorf("failed to download the layer(%s): %w", d, err)
					return
				}
				rc, err = cache.Set(string(d), rc)
				if err != nil {
					log.Print(err)
				}
			}
			gzipReader, err := gzip.NewReader(rc)
			if err != nil {
				errCh <- xerrors.Errorf("invalid gzip: %w", err)
				return
			}
			ch <- layer{ID: d, Content: gzipReader}
		}(ref.Digest)
	}

	filesInLayers := make(map[string]extractor.FileMap)
	opqInLayers := make(map[string]opqDirs)
	for i := 0; i < len(m.Manifest.Layers); i++ {
		var l layer
		select {
		case l = <-ch:
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, xerrors.Errorf("timeout: %w", ctx.Err())
		}

		files, opqDirs, err := d.ExtractFiles(l.Content, filenames)
		if err != nil {
			return nil, err
		}
		layerID := string(l.ID)
		filesInLayers[layerID] = files
		opqInLayers[layerID] = opqDirs
	}

	fileMap, err := applyLayers(layerIDs, filesInLayers, opqInLayers)
	if err != nil {
		return nil, xerrors.Errorf("failed to apply layers: %w", err)
	}

	// download config file
	rc, err := r.DownloadLayer(ctx, image.Path, m.Manifest.Config.Digest)
	if err != nil {
		return nil, xerrors.Errorf("error in layer download: %w", err)
	}
	config, err := ioutil.ReadAll(rc)
	if err != nil {
		return nil, xerrors.Errorf("failed to decode config JSON: %w", err)
	}

	// special file for command analyzer
	fileMap["/config"] = config

	return fileMap, nil
}

func (d DockerExtractor) ExtractFromFile(ctx context.Context, r io.Reader, filenames []string) (extractor.FileMap, error) {
	manifests := make([]manifest, 0)
	filesInLayers := map[string]extractor.FileMap{}
	opqInLayers := make(map[string]opqDirs)

	tarFiles := make(map[string][]byte)

	// Extract the files from the tarball
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, extractor.ErrCouldNotExtract
		}
		tarFiles[header.Name], err = ioutil.ReadAll(tr)
		if err != nil {
			return nil, err
		}
	}

	data, ok := tarFiles["manifest.json"]
	if ok == false {
		return nil, xerrors.New("manifest.json not found")
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&manifests); err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return nil, xerrors.New("Invalid manifest file")
	}

	// Extract the layers
	for _, layerPath := range manifests[0].Layers {
		data, ok := tarFiles[layerPath]
		if ok == false {
			return nil, xerrors.Errorf("Layer: %s not found in tarball\n", layerPath)
		}

		r := bytes.NewReader(data)

		switch {
		case strings.HasSuffix(layerPath, ".tar"):
			files, opqDirs, err := d.ExtractFiles(r, filenames)

			if err != nil {
				return nil, err
			}
			filesInLayers[layerPath] = files
			opqInLayers[layerPath] = opqDirs
		case strings.HasSuffix(layerPath, ".tar.gz"):
			gzipReader, err := gzip.NewReader(r)
			if err != nil {
				return nil, err
			}
			files, opqDirs, err := d.ExtractFiles(gzipReader, filenames)
			if err != nil {
				return nil, err
			}
			filesInLayers[layerPath] = files
			opqInLayers[layerPath] = opqDirs
		default:
			return nil, xerrors.Errorf("layer: %s: format not supported", layerPath)
		}
	}

	fileMap, err := applyLayers(manifests[0].Layers, filesInLayers, opqInLayers)
	if err != nil {
		return nil, err
	}

	// special file for command analyzer
	data, ok = tarFiles[manifests[0].Config]
	if ok == false {
		return nil, xerrors.Errorf("Image config: %s not found\n", manifests[0].Config)
	}
	fileMap["/config"] = data

	return fileMap, nil
}

func (d DockerExtractor) ExtractFiles(layer io.Reader, filenames []string) (extractor.FileMap, opqDirs, error) {
	data := make(map[string][]byte)
	opqDirs := opqDirs{}

	tr := tar.NewReader(layer)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return data, nil, extractor.ErrCouldNotExtract
		}

		filePath := hdr.Name
		filePath = strings.TrimLeft(filepath.Clean(filePath), "/")
		fileName := filepath.Base(filePath)

		// e.g. etc/.wh..wh..opq
		if opq == fileName {
			opqDirs = append(opqDirs, filepath.Dir(filePath))
			continue
		}

		// Determine if we should extract the element
		extract := false
		for _, s := range filenames {
			if s == filePath || s == fileName || strings.HasPrefix(fileName, wh) {
				extract = true
				break
			}
		}

		if !extract {
			continue
		}

		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink || hdr.Typeflag == tar.TypeReg {
			d, err := ioutil.ReadAll(tr)
			if err != nil {
				return nil, nil, xerrors.Errorf("failed to read file: %w", err)
			}
			data[filePath] = d
		}
	}

	return data, opqDirs, nil

}
