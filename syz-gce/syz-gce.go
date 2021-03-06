// Copyright 2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

//go:generate bash -c "echo -en '// AUTOGENERATED FILE\n\n' > generated.go"
//go:generate bash -c "echo -en 'package main\n\n' >> generated.go"
//go:generate bash -c "echo -en 'const syzconfig = `\n' >> generated.go"
//go:generate bash -c "cat kernel.config | grep -v '#' >> generated.go"
//go:generate bash -c "echo -en '`\n\n' >> generated.go"
//go:generate bash -c "echo -en 'const createImageScript = `#!/bin/bash\n' >> generated.go"
//go:generate bash -c "cat ../tools/create-gce-image.sh | grep -v '#' >> generated.go"
//go:generate bash -c "echo -en '`\n\n' >> generated.go"

// syz-gce runs syz-manager on GCE in a continous loop handling image/syzkaller updates.
// It downloads test image from GCS, downloads and builds syzkaller, then starts syz-manager
// and pulls for image/syzkaller source updates. If image/syzkaller changes,
// it stops syz-manager and starts from scratch.
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/syzkaller/config"
	"github.com/google/syzkaller/gce"
	. "github.com/google/syzkaller/log"
	"golang.org/x/net/context"
)

var (
	flagConfig = flag.String("config", "", "config file")

	cfg             *Config
	ctx             context.Context
	storageClient   *storage.Client
	GCE             *gce.Context
	managerHttpPort uint32
)

type Config struct {
	Name            string
	Hub_Addr        string
	Hub_Key         string
	Image_Archive   string
	Image_Path      string
	Image_Name      string
	Http_Port       int
	Machine_Type    string
	Machine_Count   int
	Sandbox         string
	Procs           int
	Linux_Git       string
	Linux_Branch    string
	Linux_Compiler  string
	Linux_Userspace string
}

func main() {
	flag.Parse()
	cfg = readConfig(*flagConfig)
	EnableLogCaching(1000, 1<<20)
	initHttp(fmt.Sprintf(":%v", cfg.Http_Port))

	wd, err := os.Getwd()
	if err != nil {
		Fatalf("failed to get wd: %v", err)
	}
	gopath := abs(wd, "gopath")
	os.Setenv("GOPATH", gopath)

	ctx = context.Background()
	storageClient, err = storage.NewClient(ctx)
	if err != nil {
		Fatalf("failed to create cloud storage client: %v", err)
	}

	GCE, err = gce.NewContext()
	if err != nil {
		Fatalf("failed to init gce: %v", err)
	}
	Logf(0, "gce initialized: running on %v, internal IP, %v project %v, zone %v", GCE.Instance, GCE.InternalIP, GCE.ProjectID, GCE.ZoneID)

	sigC := make(chan os.Signal, 2)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGUSR1)

	var managerCmd *exec.Cmd
	managerStopped := make(chan error)
	stoppingManager := false
	var lastImageUpdated time.Time
	lastSyzkallerHash := ""
	lastLinuxHash := ""
	buildDir := abs(wd, "build")
	linuxDir := filepath.Join(buildDir, "linux")
	var delayDuration time.Duration
	for {
		if delayDuration != 0 {
			Logf(0, "sleep for %v", delayDuration)
			select {
			case <-time.After(delayDuration):
			case err := <-managerStopped:
				if managerCmd == nil {
					Fatalf("spurious manager stop signal")
				}
				Logf(0, "syz-manager exited with %v", err)
				managerCmd = nil
				atomic.StoreUint32(&managerHttpPort, 0)
			case s := <-sigC:
				switch s {
				case syscall.SIGUSR1:
					// just poll for updates
					Logf(0, "SIGUSR1")
				case syscall.SIGINT:
					Logf(0, "SIGINT")
					if managerCmd != nil {
						Logf(0, "shutting down syz-manager...")
						managerCmd.Process.Signal(syscall.SIGINT)
						select {
						case err := <-managerStopped:
							if managerCmd == nil {
								Fatalf("spurious manager stop signal")
							}
							Logf(0, "syz-manager exited with %v", err)
						case <-sigC:
							managerCmd.Process.Kill()
						case <-time.After(time.Minute):
							managerCmd.Process.Kill()
						}
					}
					os.Exit(0)
				}
			}
		}
		delayDuration = 10 * time.Minute // assume that an error happened

		// Poll syzkaller repo.
		syzkallerHash, err := updateSyzkallerBuild()
		if err != nil {
			Logf(0, "failed to update syzkaller: %v", err)
			continue
		}

		// Poll kernel git repo or GCS image.
		var imageArchive *storage.ObjectHandle
		var imageUpdated time.Time
		linuxHash := ""
		if cfg.Image_Archive == "local" {
			if syscall.Getuid() != 0 {
				Fatalf("building local image requires root")
			}
			var err error
			linuxHash, err = gitUpdate(linuxDir, cfg.Linux_Git, cfg.Linux_Branch)
			if err != nil {
				Logf(0, "%v", err)
				delayDuration = time.Hour // cloning linux is expensive
				continue
			}
			Logf(0, "kernel hash %v, syzkaller hash %v", linuxHash, syzkallerHash)
		} else {
			var err error
			imageArchive, imageUpdated, err = openFile(cfg.Image_Archive)
			if err != nil {
				Logf(0, "%v", err)
				continue
			}
			Logf(0, "image update time %v, syzkaller hash %v", imageUpdated, syzkallerHash)
		}

		if lastImageUpdated == imageUpdated &&
			lastLinuxHash == linuxHash &&
			lastSyzkallerHash == syzkallerHash &&
			managerCmd != nil {
			// Nothing has changed, sleep for another hour.
			delayDuration = time.Hour
			continue
		}

		// At this point we are starting an update. First, stop manager.
		if managerCmd != nil {
			if !stoppingManager {
				stoppingManager = true
				Logf(0, "stopping syz-manager...")
				managerCmd.Process.Signal(syscall.SIGINT)
			} else {
				Logf(0, "killing syz-manager...")
				managerCmd.Process.Kill()
			}
			delayDuration = time.Minute
			continue
		}

		// Download and extract image from GCS.
		if lastImageUpdated != imageUpdated {
			Logf(0, "downloading image archive...")
			if err := os.RemoveAll("image"); err != nil {
				Logf(0, "failed to remove image dir: %v", err)
				continue
			}
			if err := downloadAndExtract(imageArchive, "image"); err != nil {
				Logf(0, "failed to download and extract %v: %v", cfg.Image_Archive, err)
				continue
			}

			Logf(0, "uploading image...")
			if err := uploadFile("image/disk.tar.gz", cfg.Image_Path); err != nil {
				Logf(0, "failed to upload image: %v", err)
				continue
			}

			Logf(0, "creating gce image...")
			if err := GCE.DeleteImage(cfg.Image_Name); err != nil {
				Logf(0, "failed to delete GCE image: %v", err)
				continue
			}
			if err := GCE.CreateImage(cfg.Image_Name, cfg.Image_Path); err != nil {
				Logf(0, "failed to create GCE image: %v", err)
				continue
			}
		}
		lastImageUpdated = imageUpdated

		// Rebuild kernel.
		if lastLinuxHash != linuxHash {
			Logf(0, "building linux kernel...")
			if err := buildKernel(linuxDir, cfg.Linux_Compiler); err != nil {
				Logf(0, "build failed: %v", err)
				continue
			}

			scriptFile := filepath.Join(buildDir, "create-gce-image.sh")
			if err := ioutil.WriteFile(scriptFile, []byte(createImageScript), 0700); err != nil {
				Logf(0, "failed to write script file: %v", err)
				continue
			}

			Logf(0, "building image...")
			vmlinux := filepath.Join(linuxDir, "vmlinux")
			bzImage := filepath.Join(linuxDir, "arch/x86/boot/bzImage")
			if _, err := runCmd(buildDir, scriptFile, abs(wd, cfg.Linux_Userspace), bzImage, vmlinux, linuxHash); err != nil {
				Logf(0, "image build failed: %v", err)
				continue
			}
			os.Remove(filepath.Join(buildDir, "disk.raw"))
			os.Remove(filepath.Join(buildDir, "image.tar.gz"))
			os.MkdirAll("image/obj", 0700)
			if err := ioutil.WriteFile("image/tag", []byte(linuxHash), 0600); err != nil {
				Logf(0, "failed to write tag file: %v", err)
				continue
			}
			if err := os.Rename(filepath.Join(buildDir, "key"), "image/key"); err != nil {
				Logf(0, "failed to rename key file: %v", err)
				continue
			}
			if err := os.Rename(vmlinux, "image/obj/vmlinux"); err != nil {
				Logf(0, "failed to rename vmlinux file: %v", err)
				continue
			}
			Logf(0, "uploading image...")
			if err := uploadFile(filepath.Join(buildDir, "disk.tar.gz"), cfg.Image_Path); err != nil {
				Logf(0, "failed to upload image: %v", err)
				continue
			}

			Logf(0, "creating gce image...")
			if err := GCE.DeleteImage(cfg.Image_Name); err != nil {
				Logf(0, "failed to delete GCE image: %v", err)
				continue
			}
			if err := GCE.CreateImage(cfg.Image_Name, cfg.Image_Path); err != nil {
				Logf(0, "failed to create GCE image: %v", err)
				continue
			}
		}
		lastLinuxHash = linuxHash

		// Rebuild syzkaller.
		if lastSyzkallerHash != syzkallerHash {
			Logf(0, "building syzkaller...")
			if _, err := runCmd("gopath/src/github.com/google/syzkaller", "make"); err != nil {
				Logf(0, "failed to update/build syzkaller: %v", err)
				continue
			}
		}
		lastSyzkallerHash = syzkallerHash

		// Restart syz-manager.
		port, err := chooseUnusedPort()
		if err != nil {
			Logf(0, "failed to choose an unused port: %v", err)
			continue
		}
		if err := writeManagerConfig(port, "manager.cfg"); err != nil {
			Logf(0, "failed to write manager config: %v", err)
			continue
		}

		Logf(0, "starting syz-manager...")
		managerCmd = exec.Command("gopath/src/github.com/google/syzkaller/bin/syz-manager", "-config=manager.cfg")
		if err := managerCmd.Start(); err != nil {
			Logf(0, "failed to start syz-manager: %v", err)
			managerCmd = nil
			continue
		}
		stoppingManager = false
		atomic.StoreUint32(&managerHttpPort, uint32(port))
		go func() {
			managerStopped <- managerCmd.Wait()
		}()
		delayDuration = 6 * time.Hour
	}
}

func readConfig(filename string) *Config {
	if filename == "" {
		Fatalf("supply config in -config flag")
	}
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		Fatalf("failed to read config file: %v", err)
	}
	cfg := new(Config)
	if err := json.Unmarshal(data, cfg); err != nil {
		Fatalf("failed to parse config file: %v", err)
	}
	return cfg
}

func writeManagerConfig(httpPort int, file string) error {
	tag, err := ioutil.ReadFile("image/tag")
	if err != nil {
		return fmt.Errorf("failed to read tag file: %v", err)
	}
	if len(tag) != 0 && tag[len(tag)-1] == '\n' {
		tag = tag[:len(tag)-1]
	}
	managerCfg := &config.Config{
		Name:         cfg.Name,
		Hub_Addr:     cfg.Hub_Addr,
		Hub_Key:      cfg.Hub_Key,
		Http:         fmt.Sprintf(":%v", httpPort),
		Rpc:          ":0",
		Workdir:      "workdir",
		Vmlinux:      "image/obj/vmlinux",
		Tag:          string(tag),
		Syzkaller:    "gopath/src/github.com/google/syzkaller",
		Type:         "gce",
		Machine_Type: cfg.Machine_Type,
		Count:        cfg.Machine_Count,
		Image:        cfg.Image_Name,
		Sandbox:      cfg.Sandbox,
		Procs:        cfg.Procs,
		Cover:        true,
	}
	if _, err := os.Stat("image/key"); err == nil {
		managerCfg.Sshkey = "image/key"
	}
	data, err := json.MarshalIndent(managerCfg, "", "\t")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(file, data, 0600); err != nil {
		return err
	}
	return nil
}

func chooseUnusedPort() (int, error) {
	ln, err := net.Listen("tcp4", ":")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

func openFile(file string) (*storage.ObjectHandle, time.Time, error) {
	pos := strings.IndexByte(file, '/')
	if pos == -1 {
		return nil, time.Time{}, fmt.Errorf("invalid GCS file name: %v", file)
	}
	bkt := storageClient.Bucket(file[:pos])
	f := bkt.Object(file[pos+1:])
	attrs, err := f.Attrs(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to read %v attributes: %v", file, err)
	}
	if !attrs.Deleted.IsZero() {
		return nil, time.Time{}, fmt.Errorf("file %v is deleted", file)
	}
	f = f.If(storage.Conditions{
		GenerationMatch:     attrs.Generation,
		MetagenerationMatch: attrs.MetaGeneration,
	})
	return f, attrs.Updated, nil
}

func downloadAndExtract(f *storage.ObjectHandle, dir string) error {
	r, err := f.NewReader(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	files := make(map[string]bool)
	ar := tar.NewReader(gz)
	for {
		hdr, err := ar.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		Logf(0, "extracting file: %v (%v bytes)", hdr.Name, hdr.Size)
		if len(hdr.Name) == 0 || hdr.Name[len(hdr.Name)-1] == '/' {
			continue
		}
		files[filepath.Clean(hdr.Name)] = true
		base, file := filepath.Split(hdr.Name)
		if err := os.MkdirAll(filepath.Join(dir, base), 0700); err != nil {
			return err
		}
		dst, err := os.OpenFile(filepath.Join(dir, base, file), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, ar)
		dst.Close()
		if err != nil {
			return err
		}
	}
	for _, need := range []string{"disk.tar.gz", "tag", "obj/vmlinux"} {
		if !files[need] {
			return fmt.Errorf("archive misses required file '%v'", need)
		}
	}
	return nil
}

func uploadFile(localFile string, gcsFile string) error {
	local, err := os.Open(localFile)
	if err != nil {
		return err
	}
	defer local.Close()
	pos := strings.IndexByte(gcsFile, '/')
	if pos == -1 {
		return fmt.Errorf("invalid GCS file name: %v", gcsFile)
	}
	bkt := storageClient.Bucket(gcsFile[:pos])
	f := bkt.Object(gcsFile[pos+1:])
	w := f.NewWriter(ctx)
	defer w.Close()
	io.Copy(w, local)
	return nil
}

// updateSyzkallerBuild executes 'git pull' on syzkaller and all depenent packages.
// Returns syzkaller HEAD hash.
func updateSyzkallerBuild() (string, error) {
	cmd := exec.Command("go", "get", "-u", "-d", "github.com/google/syzkaller/syz-manager")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v\n%s", err, output)
	}
	return gitRevision("gopath/src/github.com/google/syzkaller")
}

func gitUpdate(dir, repo, branch string) (string, error) {
	if _, err := runCmd(dir, "git", "pull"); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("failed to remove repo dir: %v", err)
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", fmt.Errorf("failed to create repo dir: %v", err)
		}
		if _, err := runCmd("", "git", "clone", repo, dir); err != nil {
			return "", err
		}
		if _, err := runCmd(dir, "git", "pull"); err != nil {
			return "", err
		}
	}
	if branch != "" {
		if _, err := runCmd(dir, "git", "checkout", branch); err != nil {
			return "", err
		}
	}
	return gitRevision(dir)
}

func gitRevision(dir string) (string, error) {
	output, err := runCmd(dir, "git", "log", "--pretty=format:'%H'", "-n", "1")
	if err != nil {
		return "", err
	}
	if len(output) != 0 && output[len(output)-1] == '\n' {
		output = output[:len(output)-1]
	}
	if len(output) != 0 && output[0] == '\'' && output[len(output)-1] == '\'' {
		output = output[1 : len(output)-1]
	}
	if len(output) != 40 {
		return "", fmt.Errorf("unexpected git log output, want commit hash: %q", output)
	}
	return string(output), nil
}

func buildKernel(dir, ccompiler string) error {
	os.Remove(filepath.Join(dir, ".config"))
	if _, err := runCmd(dir, "make", "defconfig"); err != nil {
		return err
	}
	if _, err := runCmd(dir, "make", "kvmconfig"); err != nil {
		return err
	}
	configFile := filepath.Join(dir, "syz.config")
	if err := ioutil.WriteFile(configFile, []byte(syzconfig), 0600); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}
	if _, err := runCmd(dir, "scripts/kconfig/merge_config.sh", "-n", ".config", configFile); err != nil {
		return err
	}
	if _, err := runCmd(dir, "make", "olddefconfig"); err != nil {
		return err
	}
	if _, err := runCmd(dir, "make", "-j", strconv.Itoa(runtime.NumCPU()*2), "CC="+ccompiler); err != nil {
		return err
	}
	return nil
}

func runCmd(dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run %v %+v: %v\n%s", bin, args, err, output)
	}
	return output, nil
}

func abs(wd, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(wd, path)
	}
	return path
}
