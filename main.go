package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	CGreen            = "\033[32m"
	CEnd              = "\033[0m"
	ImagesPath        = ".dockie/images"
	ContainerDataPath = ".dockie/containers"
)

func main() {
	imagesDir := must(filepath.Abs(ImagesPath))
	noErr(os.MkdirAll(imagesDir, 0755))
	containersDir := must(filepath.Abs(ContainerDataPath))
	noErr(os.MkdirAll(containersDir, 0755))
	s := strings.Split(os.Args[2], ":")
	image := Image{
		Name: s[0],
		Tag:  s[1],
	}

	switch os.Args[1] {
	case "pull":
		Pull(image)
	case "run":
		Run()
	case "child":
		command := os.Args[3]
		conf := RunConfig{}
		conf.SetCpus(0.25)
		conf.SetMem("128M")
		RunChild(image, command, conf)
	default:
		panic("???")
	}
}

// Pull an image from the registry and unpack the layers
func Pull(image Image) {
	image.setupImageDir()
	log.Printf("pulling %s:%s", image.Name, image.Tag)

	image.loadToken()
	image.downloadLayers()

	log.Printf("%spulled👌%s %s:%s", CGreen, CEnd, image.Name, image.Tag)
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
}

func (image *Image) setupImageDir() {
	imagesDir := must(filepath.Abs(ImagesPath))
	imageDir := filepath.Join(imagesDir, image.Name)
	initDir(imageDir)
	layersDir := filepath.Join(imageDir, "layers")
	initDir(layersDir)
	contentsDir := filepath.Join(layersDir, "contents")
	initDir(contentsDir)
	image.Dir = ImageDirs{
		ImageDir:    imageDir,
		LayersDir:   layersDir,
		ContentsDir: contentsDir,
	}
}

// loadToken retrieves and sets a Bearer token to pull images from the container registry
// See also: https://distribution.github.io/distribution/spec/auth/token/
func (image *Image) loadToken() {
	url := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/%s:pull", image.Name)
	log.Printf("fetching token from %s", url)
	res := must(http.Get(url))
	if res.StatusCode != http.StatusOK {
		log.Panicf("failed to fetch token. status=%s", res.Status)
	}
	defer func() { _ = res.Body.Close() }()
	body := must(io.ReadAll(res.Body))
	var tres struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tres); err != nil {
		log.Panicf("failed to unmarshal token response. body=%v, err=%v", body, err)
	}
	image.Token = tres.Token
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

// downloadLayers downloads the layers of the image and unpacks them
func (image *Image) downloadLayers() {
	manifests := func() []Manifest {
		url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/manifests/%s", image.Name, image.Tag)
		log.Printf("fetching manifests from %s", url)
		res := fetch(url, image.Token)

		body := must(io.ReadAll(res.Body))
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
	}()

	layerDigests := make([]string, 0)
	for _, manifest := range manifests {
		switch manifest.MediaType {
		case "application/vnd.oci.image.manifest.v1+json":
			layers := func() []Layer {
				url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/blobs/%s", image.Name, manifest.Digest)
				log.Printf("fetching manifest from %s", url)
				res := fetch(url, image.Token)

				body := must(io.ReadAll(res.Body))
				var mres struct {
					Layers []Layer `json:"layers"`
				}
				if err := json.Unmarshal(body, &mres); err != nil {
					log.Panicf("failed to unmarshal Manifest response. url=%s, body=%v, err=%v", url, body, err)
				}

				return mres.Layers
			}()
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
		// download
		log.Printf("downloading layer %s", digest)
		f := must(os.Create(filepath.Join(image.Dir.LayersDir, digest+".tar.gz")))
		url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/blobs/%s", image.Name, digest)
		res := fetch(url, image.Token)
		_ = must(io.Copy(f, res.Body))
		// unpack
		tarFile := must(filepath.Abs(f.Name()))
		unpackDir := filepath.Join(image.Dir.ContentsDir, image.Tag)
		initDir(unpackDir)
		noErr(exec.Command("tar", "zxvf", tarFile, "-C", unpackDir).Run())
	}
}

// Run starts a container with the specified image and command
func Run() {
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)
	cmd.SysProcAttr = &unix.SysProcAttr{
		// https://gihyo.jp/admin/serial/01/linux_containers/0002
		Cloneflags: unix.CLONE_NEWUTS | // hostname & domain name
			unix.CLONE_NEWPID | // PID namespace
			unix.CLONE_NEWNS, // mount namespace
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	noErr(cmd.Run())
}

type RunConfig struct {
	Cpus *float32
	Mem  *string
}

func (conf *RunConfig) SetCpus(cpus float32) {
	conf.Cpus = &cpus
}
func (conf *RunConfig) SetMem(mem string) {
	conf.Mem = &mem
}

func RunChild(image Image, command string, conf RunConfig) {
	containerId := fmt.Sprintf("dockie_%s_%s", image.Name, image.Tag)
	dataDir := must(filepath.Abs(ContainerDataPath))
	rootDir := filepath.Join(dataDir, containerId)
	initDir(rootDir)
	rootFsDir := filepath.Join(rootDir, "rootfs")
	initDir(rootFsDir)
	rwDir := filepath.Join(rootDir, "cow_rw")
	initDir(rwDir)
	workDir := filepath.Join(rootDir, "cow_workdir")
	initDir(workDir)

	/*
		// コンテナの最大CPU利用量を制限
		//See: https://gihyo.jp/admin/serial/01/linux_containers/0004?page=2
		cgCpuDir := filepath.Join(CGroupCpuDir, "dockie", containerId)
		initDir(cgCpuDir)
		cgCpuFile := must(os.Create(filepath.Join(cgCpuDir, "tasks")))
		must(cgCpuFile.WriteString(strconv.Itoa(os.Getpid())))
		if conf.Cpus != nil {
			cpuLimit := int(*conf.Cpus * 100000) // CPU時間の割当周期は100msとする
			cpuQuotaFile := must(os.Create(filepath.Join(cgCpuDir, "cpu.cfs_quota_us")))
			must(cpuQuotaFile.WriteString(strconv.Itoa(cpuLimit)))
			log.Printf("set cpu quota %d", cpuLimit)
		}

		// コンテナの最大メモリ利用量を制限
		cgMemDir := filepath.Join(CGroupMemDir, "dockie", containerId)
		initDir(cgMemDir)
		cgMemFile := must(os.Create(filepath.Join(cgMemDir, "tasks")))
		must(cgMemFile.WriteString(strconv.Itoa(os.Getpid())))
		if conf.Mem != nil {
			memLimitFile := must(os.Create(filepath.Join(cgMemDir, "memory.limit_in_bytes")))
			must(memLimitFile.WriteString(*conf.Mem))
			memSwapLimitFile := must(os.Create(filepath.Join(cgMemDir, "memory.memsw.limit_in_bytes"))) // swapも制限する
			must(memSwapLimitFile.WriteString(*conf.Mem))
			log.Printf("set memory limit %s", *conf.Mem)
		}*
	*/

	// ホスト名をセット
	noErr(unix.Sethostname([]byte(containerId)))

	// ルートディレクトリをプライベートにマウント
	// https://kernhack.hatenablog.com/entry/2015/05/30/115705
	// https://www.kernel.org/doc/html/latest/filesystems/sharedsubtree.html
	noErr(unix.Mount("rootfs", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))

	// docker imageのディレクトリをマウント
	imagesDir := must(filepath.Abs(ImagesPath))
	imageDir := filepath.Join(imagesDir, image.Name, "layers", "contents", image.Tag)
	noErr(unix.Mount(
		"overlay",
		rootDir,
		"overlay",
		unix.MS_NODEV,
		fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", imageDir, rwDir, workDir)),
	)
	// システムディレクトリを構成
	// /proc: PIDなどプロセスの情報
	procDir := filepath.Join(rootDir, "proc")
	initDir(procDir)
	noErr(unix.Mount("proc", procDir, "proc", 0, ""))

	// /sys: ドライバ関連のプロセスの情報
	sysDir := filepath.Join(rootDir, "sys")
	initDir(sysDir)
	noErr(unix.Mount("sysfs", sysDir, "sysfs", 0, ""))

	// /dev: dev: CPUやメモリなど基本デバイス
	devDir := filepath.Join(rootDir, "dev")
	initDir(devDir)
	noErr(unix.Mount("tmpfs", devDir, "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=755"))
	// /dev/null
	noErr(unix.Mknod(filepath.Join(devDir, "null"), unix.S_IFCHR|0666, int(unix.Mkdev(1, 3))))
	// /dev/tty
	noErr(unix.Mknod(filepath.Join(devDir, "tty"), unix.S_IFCHR|0666, int(unix.Mkdev(5, 0))))
	// /dev/random
	noErr(unix.Mknod(filepath.Join(devDir, "random"), unix.S_IFCHR|0666, int(unix.Mkdev(1, 8))))

	// pivot_root: 新しいルートディレクトリをセット
	oldRoot := filepath.Join(rootDir, "oldroot")
	initDir(oldRoot)
	noErr(unix.PivotRoot(rootDir, oldRoot))
	noErr(unix.Chdir("/"))
	noErr(unix.Unmount("/oldroot", unix.MNT_DETACH))

	cmd := exec.Command(command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	noErr(cmd.Run())
}

func fetch(url, token string) *http.Response {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	res := must(http.DefaultClient.Do(req))
	if res.StatusCode != http.StatusOK {
		log.Panicf("failed to fetch %s. status=%s", url, res.Status)
	}
	return res
}

func must[T any](obj T, err error) T {
	if err != nil {
		panic(err)
	}
	return obj
}

func noErr(err error) {
	if err != nil {
		panic(err)
	}
}

func initDir(dir string) {
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		noErr(os.RemoveAll(dir))
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		noErr(os.MkdirAll(dir, 0755))
	}
}
