package main

import (
	"bufio"
	"bytes"
	crand "crypto/rand"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"github.com/philandstuff/dhall-golang/v5"
	"golang.org/x/tools/txtar"
)

//go:embed data/* templates/*
var data embed.FS

var (
	distro     = flag.String("distro", "alpine-edge", "the linux distro to install in the VM")
	name       = flag.String("name", "", "the name of the VM, defaults to a random common blade name")
	zvolPrefix = flag.String("zvol-prefix", "rpool/mkvm-test/", "the prefix to use for zvol names")
	zvolSize   = flag.Int("zvol-size", 0, "the number of gigabytes for the virtual machine disk")
	memory     = flag.Int("memory", 512, "the number of megabytes of ram for the virtual machine")
)

func main() {
	rand.Seed(time.Now().Unix())
	flag.Parse()

	if *name == "" {
		commonBladeName, err := getName()
		if err != nil {
			log.Fatal(err)
		}
		name = &commonBladeName
	}

	distros, err := getDistros()
	if err != nil {
		log.Fatalf("can't load internal list of distros: %v", err)
	}

	var resultDistro Distro
	var found bool
	for _, d := range distros {
		if d.Name == *distro {
			found = true
			resultDistro = d
			if *zvolSize == 0 {
				zvolSize = &d.MinSize
			}
			if *zvolSize < d.MinSize {
				zvolSize = &d.MinSize
			}
		}
	}
	if !found {
		fmt.Printf("can't find distro %s in my list. Here are distros I know about:\n", *distro)
		for _, d := range distros {
			fmt.Println(d.Name)
		}
		os.Exit(1)
	}
	zvol := filepath.Join(*zvolPrefix, *name)

	macAddress, err := randomMac()
	if err != nil {
		log.Fatalf("can't generate mac address: %v", err)
	}

	l, err := connectToLibvirt()
	if err != nil {
		log.Fatalf("can't connect to libvirt: %v", err)
	}

	log.Println("plan:")
	log.Printf("name: %s", *name)
	log.Printf("zvol: %s (%d GB)", zvol, *zvolSize)
	log.Printf("base image url: %s", resultDistro.DownloadURL)
	log.Printf("mac address: %s", macAddress)
	log.Printf("ram: %d MB", *memory)

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("press enter if this looks okay:")
	reader.ReadString('\n')

	cdir, err := os.UserCacheDir()
	if err != nil {
		log.Fatalf("can't find cache dir: %v", err)
	}
	cdir = filepath.Join(cdir, "within", "mkvm")
	os.MkdirAll(filepath.Join(cdir, "qcow2"), 0755)
	os.MkdirAll(filepath.Join(cdir, "seed"), 0755)
	qcowPath := filepath.Join(cdir, "qcow2", resultDistro.Sha256Sum)
	_, err = os.Stat(qcowPath)
	if err != nil {
		log.Printf("downloading distro image %s to %s", resultDistro.DownloadURL, qcowPath)
		fout, err := os.Create(qcowPath)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := http.Get(resultDistro.DownloadURL)
		if err != nil {
			log.Fatalf("can't fetch qcow2 for %s (%s): %v", resultDistro.Name, resultDistro.DownloadURL, err)
		}

		if resp.StatusCode != http.StatusOK {
			log.Fatalf("%s replied %s", resultDistro.DownloadURL, resp.Status)
		}

		_, err = io.Copy(fout, resp.Body)
		if err != nil {
			log.Fatalf("download of %s failed: %v", resultDistro.DownloadURL, err)
		}

		fout.Close()
		resp.Body.Close()
	}

	tmpl := template.Must(template.ParseFS(data, "templates/*"))
	var buf = bytes.NewBuffer(nil)
	err = tmpl.ExecuteTemplate(buf, "cloud-config.txtar", struct{ Name string }{Name: *name})
	if err != nil {
		log.Fatalf("can't generate cloud-config: %v", err)
	}

	arc := txtar.Parse(buf.Bytes())
	dir, err := os.MkdirTemp("", "mkvm")
	if err != nil {
		log.Fatalf("can't make directory: %v", err)
	}

	for _, file := range arc.Files {
		fout, err := os.Create(filepath.Join(dir, file.Name))
		if err != nil {
			log.Fatal(err)
		}
		_, err = fout.Write(file.Data)
		if err != nil {
			log.Fatal(err)
		}
	}

	isoPath := filepath.Join(cdir, "seed", fmt.Sprintf("%s-%s.iso", *name, resultDistro.Name))

	err = run(
		"genisoimage",
		"-output",
		isoPath,
		"-volid",
		"cidata",
		"-joliet",
		"-rock",
		filepath.Join(dir, "meta-data"),
		filepath.Join(dir, "user-data"),
	)
	if err != nil {
		log.Fatal(err)
	}

	ram := *memory * 1024
	vmID := uuid.New().String()
	buf.Reset()

	// zfs create -V 20G rpool/safe/vm/sena
	err = run("sudo", "zfs", "create", "-V", fmt.Sprintf("%dG", *zvolSize), zvol)
	if err != nil {
		log.Fatalf("can't create zvol %s: %v", zvol, err)
	}

	err = run("sudo", "qemu-img", "convert", "-O", "raw", qcowPath, filepath.Join("/dev/zvol", zvol))
	if err != nil {
		log.Fatalf("can't import qcow2: %v", err)
	}

	err = tmpl.ExecuteTemplate(buf, "base.xml", struct {
		Name       string
		UUID       string
		Memory     int
		ZVol       string
		Seed       string
		MACAddress string
	}{
		Name:       *name,
		UUID:       vmID,
		Memory:     ram,
		ZVol:       zvol,
		Seed:       isoPath,
		MACAddress: macAddress,
	})
	if err != nil {
		log.Fatalf("can't generate VM template: %v", err)
	}

	domain, err := mkVM(l, buf)
	if err != nil {
		log.Printf("can't create domain for %s: %v", *name, err)
		log.Println("you should run this command:")
		log.Println()
		log.Printf("zfs destroy %s", zvol)
		os.Exit(1)
	}

	log.Printf("created %s", domain.Name)
}

func randomMac() (string, error) {
	buf := make([]byte, 6)
	_, err := crand.Read(buf)
	if err != nil {
		return "", err
	}

	buf[0] = (buf[0] | 2) & 0xfe

	return net.HardwareAddr(buf).String(), nil
}

func getName() (string, error) {
	var names []string
	nameData, err := data.ReadFile("data/names.json")
	if err != nil {
		return "", err
	}

	err = json.Unmarshal(nameData, &names)
	if err != nil {
		return "", err
	}

	return names[rand.Intn(len(names))], nil
}

func run(args ...string) error {
	log.Println("running command:", strings.Join(args, " "))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func connectToLibvirt() (*libvirt.Libvirt, error) {
	c, err := net.DialTimeout("unix", "/var/run/libvirt/libvirt-sock", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("can't dial libvirt: %w", err)
	}

	l := libvirt.New(c)

	_, err = l.AuthPolkit()
	if err != nil {
		return nil, fmt.Errorf("can't auth with polkit: %w", err)
	}

	if err := l.Connect(); err != nil {
		return nil, fmt.Errorf("can't connect: %w", err)
	}

	return l, nil
}

func mkVM(l *libvirt.Libvirt, buf *bytes.Buffer) (*libvirt.Domain, error) {
	domain, err := l.DomainCreateXML(buf.String(), libvirt.DomainNone)
	return &domain, err
}

type Distro struct {
	Name        string `dhall:"name" json:"name"`
	DownloadURL string `dhall:"downloadURL" json:"download_url"`
	Sha256Sum   string `dhall:"sha256Sum" json:"sha256_sum"`
	MinSize     int    `dhall:"minSize" json:"min_size"`
}

func getDistros() ([]Distro, error) {
	distroData, err := data.ReadFile("data/distros.dhall")
	if err != nil {
		return nil, err
	}

	var result []Distro
	err = dhall.Unmarshal(distroData, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}
