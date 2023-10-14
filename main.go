package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

func main() {
	switch os.Args[1] {
	case "pull":
		Pull(os.Args[2])
	case "run":
		fmt.Print("run!")

	default:
		panic("???")
	}
}

const baseDir = "./images"
const cGreen = "\033[32m"
const cEnd = "\033[0m"

func Pull(name string) {
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		fmt.Printf("creating %s\n", baseDir)
		must(os.MkdirAll(baseDir, 0755))
	}

	re := regexp.MustCompile(`(?P<image>[^/:]+):?(?P<tag>[^/:]*)`)
	m := re.FindStringSubmatch(name)
	if m == nil {
		panic("invalid arg")
	}
	image := m[1]
	tag := m[2]
	fmt.Printf("Pulling %q: %q ...\n", image, tag)

	imageDir := filepath.Join(baseDir, image)
	if _, err := os.Stat(imageDir); os.IsNotExist(err) {
		os.Mkdir(imageDir, 0755)
	}
	if _, err := os.Stat(imageDir); !os.IsNotExist(err) {
		os.RemoveAll(imageDir)
	}

	layersDir := filepath.Join(imageDir, "layers")
	if _, err := os.Stat(layersDir); os.IsNotExist(err) {
		os.MkdirAll(layersDir, 0755)
	}

	contentsDir := filepath.Join(imageDir, "contents")
	if _, err := os.Stat(contentsDir); os.IsNotExist(err) {
		os.Mkdir(contentsDir, 0755)
	}

	token := fetchToken(image)
	man := fetchManifest(image, tag, token)
	for _, l := range man.FsLayers {
		tarPath := fetchLayerTar(image, l.BlobSum, layersDir, token)
		untar(tarPath, contentsDir)
	}
	fmt.Printf("%simage %s:%s has been pulled%s 👌\n", cGreen, image, tag, cEnd)
}

// Fetch auth token from Docker Hub
// See also: https://docs.docker.com/registry/spec/auth/jwt/
func fetchToken(image string) string {
	url := "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/" + image + ":pull"
	res, err := http.Get(url)
	must(err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(err)
	var m map[string]string
	json.Unmarshal(body, &m)
	return m["token"]
}

func fetchManifest(image, tag, token string) *manifest {
	fmt.Printf("Fetching manifest for %q:%q ...\n", image, tag)
	url := "https://registry-1.docker.io/v2/library/" + image + "/manifests/" + tag
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	c := new(http.Client)
	res, err := c.Do(req)
	must(err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(err)
	m := manifest{}
	json.Unmarshal(body, &m)
	return &m
}

func fetchLayerTar(image, blobSum, dir, token string) string {
	tar := filepath.Join(dir, blobSum+".tar")
	url := "https://registry-1.docker.io/v2/library/" + image + "/blobs/" + blobSum
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	c := new(http.Client)
	res, err := c.Do(req)
	must(err)
	defer res.Body.Close()

	f, err := os.Create(tar)
	must(err)
	defer f.Close()

	if io.Copy(f, res.Body); err != nil {
		panic(err)
	}
	path, _ := filepath.Abs(f.Name())
	return path
}

func untar(path, dst string) {
	f, err := os.Open(path)
	must(err)
	defer f.Close()

	zr, err := gzip.NewReader(bufio.NewReader(f))
	must(err)
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(err)

		fi := hdr.FileInfo()
		dir := filepath.Join(dst, hdr.Name)
		fName := filepath.Join(dir, fi.Name())
		err = os.MkdirAll(dir, 0755)
		must(err)

		f, err := os.Create(fName)
		must(err)

		w := bufio.NewWriter(f)
		buf := make([]byte, 4096)
		for {
			n, err := tr.Read(buf)
			if err == io.EOF {
				break
			}
			must(err)

			_, err = w.Write(buf[:n])
			must(err)
		}
		err = w.Flush()
		must(err)

		err = f.Close()
		must(err)
	}
}

type manifest struct {
	Name     string `json:"name"`
	Tag      string `json:"tag"`
	FsLayers []struct {
		BlobSum string `json:"blobSum"`
	} `json:"fsLayers"`
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
