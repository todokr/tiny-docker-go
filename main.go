package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const cGreen = "\033[32m"
const cEnd = "\033[0m"
const baseDir = "./.images"

func main() {
	_ = os.MkdirAll(baseDir, 0755)

	switch os.Args[1] {
	case "pull":
		s := strings.Split(os.Args[2], ":")
		image := Image{
			Name: s[0],
			Tag:  s[1],
		}
		Pull(image)
	case "run":
		fmt.Print("run!")

	default:
		panic("???")
	}
}

// Pull an image from the registry and unpack the layers
func Pull(image Image) {
	image.SetupDirs()
	log.Printf("pulling %s:%s", image.Name, image.Tag)

	image.LoadToken()
	image.DownloadLayers()

	log.Printf("%spulled👌%s %s:%s", cGreen, cEnd, image.Name, image.Tag)
}

type Image struct {
	Name  string
	Tag   string
	Dir   ImageDirs
	Token string
}

type ImageDirs struct {
	// ImageDir is the directory for the image
	ImageDir string
	// LayersDir is the directory for all layers
	LayersDir string
	// ContentsDir is the directory for all contents
	ContentsDir string

	TempDir string
}

// LoadToken retrieves and sets a Bearer token to pull images from the container registry
// See also: https://distribution.github.io/distribution/spec/auth/token/
func (image *Image) LoadToken() {
	url := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/%s:pull", image.Name)
	log.Printf("fetching token from %s", url)
	res := Must(http.Get(url))
	if res.StatusCode != http.StatusOK {
		log.Panicf("failed to fetch token. status=%s", res.Status)
	}
	defer func() { _ = res.Body.Close() }()
	body := Must(io.ReadAll(res.Body))
	var tres struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tres); err != nil {
		log.Panicf("failed to unmarshal token response. body=%v, err=%v", body, err)
	}
	image.Token = tres.Token
}

func (image *Image) SetupDirs() {
	imageDir := filepath.Join(baseDir, image.Name)
	initDir(imageDir)

	layersDir := filepath.Join(imageDir, "layers")
	initDir(layersDir)

	contentsDir := filepath.Join(imageDir, "contents")
	initDir(contentsDir)

	tempDir := filepath.Join(imageDir, "temp")
	initDir(tempDir)

	image.Dir = ImageDirs{
		ImageDir:    imageDir,
		LayersDir:   layersDir,
		ContentsDir: contentsDir,
		TempDir:     tempDir,
	}
}

type Manifest struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
	PlatForm  struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
}

type Layer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

// DownloadLayers downloads the layers of the image and unpacks them
func (image *Image) DownloadLayers() {
	manifests := fetchManifests(image.Name, image.Tag, image.Token)
	layerDigests := make([]string, 0)
	for _, manifest := range manifests {
		switch manifest.MediaType {
		case "application/vnd.oci.image.manifest.v1+json":
			layers := fetchManifest(image.Name, manifest.Digest, image.Token)
			for _, layer := range layers {
				if layer.MediaType != "application/vnd.oci.image.layer.v1.tar+gzip" {
					log.Printf("skipping media type %q (%s)\n", layer.MediaType, layer.Digest)
					continue
				}
				layerDigests = append(layerDigests, layer.Digest)
			}
		case "application/vnd.docker.image.rootfs.diff.tar.gzip":
			layerDigests = append(layerDigests, manifest.Digest)
		default:
			log.Printf("unknown media type %q for %s:%s\n", manifest.MediaType, image.Name, image.Tag)
		}
	}
	for _, digest := range layerDigests {
		blobSum := strings.Split(digest, ":")[1]
		log.Printf("downloading layer %s", blobSum)
		tarFile := downloadLayerTar(image.Name, "sha256:"+blobSum, image.Dir.TempDir, image.Token)
		untarLayer(tarFile, fmt.Sprintf("%s/%s", image.Dir.LayersDir, blobSum))
	}
}

// fetchManifests retrieves the manifest for the image
// See : https://distribution.github.io/distribution/spec/manifest-v2-2/
func fetchManifests(name string, tag string, token string) []Manifest {
	url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/manifests/%s", name, tag)
	log.Printf("fetching manifests from %s", url)
	res := fetch(url, token)
	defer res.Body.Close()

	body := Must(io.ReadAll(res.Body))
	var mres struct {
		Manifests []Manifest `json:"manifests"`
	}
	if err := json.Unmarshal(body, &mres); err != nil {
		log.Panicf("failed to unmarshal Manifest response. url=%s, body=%v, err=%v", url, body, err)
	}
	manifests := make([]Manifest, 0)
	for _, manifest := range mres.Manifests {
		if manifest.PlatForm.OS == "linux" && manifest.PlatForm.Architecture == "amd64" {
			manifests = append(manifests, manifest)
		}
	}
	return manifests
}

func fetchManifest(name, digest, token string) []Layer {
	url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/blobs/%s", name, digest)
	log.Printf("fetching manifest from %s", url)
	res := fetch(url, token)
	defer res.Body.Close()

	body := Must(io.ReadAll(res.Body))
	var mres struct {
		Layers []Layer `json:"layers"`
	}
	if err := json.Unmarshal(body, &mres); err != nil {
		log.Panicf("failed to unmarshal Manifest response. url=%s, body=%v, err=%v", url, body, err)
	}

	return mres.Layers
}

func downloadLayerTar(image, blobSum, dir, token string) string {
	tarFile := Must(os.Create(filepath.Join(dir, blobSum+".tar.gz")))
	defer tarFile.Close()

	url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/blobs/%s", image, blobSum)
	res := fetch(url, token)
	defer res.Body.Close()

	_ = Must(io.Copy(tarFile, res.Body))
	path := Must(filepath.Abs(tarFile.Name()))
	return path
}

func untarLayer(path, destFile string) {
	f := Must(os.Open(path))
	defer f.Close()
	zr := Must(gzip.NewReader(f))
	defer zr.Close()

	tarReader := tar.NewReader(zr)
	for {
		_, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Panicf("failed to read tar header. err=%v", err)
		}
		destFile := Must(os.Create(destFile))
		_ = Must(io.Copy(destFile, tarReader))
	}
}

func fetch(url, token string) *http.Response {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	res := Must(http.DefaultClient.Do(req))
	if res.StatusCode != http.StatusOK {
		log.Panicf("failed to fetch %s. status=%s", url, res.Status)
	}
	return res
}

func Must[T any](obj T, err error) T {
	if err != nil {
		panic(err)
	}
	return obj
}

func initDir(dir string) {
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		if err := os.RemoveAll(dir); err != nil {
			log.Panicf("failed to remove %s. err=%v", dir, err)
		}
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0755); err != nil {
			log.Panicf("failed to create %s. err=%v", dir, err)
		}
	}
}
