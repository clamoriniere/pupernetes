// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

package setup

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/coreos/go-systemd/dbus"
	"github.com/coreos/go-systemd/unit"
	"github.com/docker/docker/client"
	"github.com/golang/glog"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/DataDog/pupernetes/pkg/config"
	"github.com/DataDog/pupernetes/pkg/options"
	"github.com/DataDog/pupernetes/pkg/setup/requirements"
	defaultTemplates "github.com/DataDog/pupernetes/pkg/setup/templates"
	"github.com/DataDog/pupernetes/pkg/util"
)

const (
	// KubeletCRILogPath isn't configurable, it's a directory where the kubelet
	// store Pods/containers logs
	KubeletCRILogPath = "/var/log/pods/"

	defaultBinaryDirName          = "bin"
	defaultSourceTemplatesDirName = "source-templates"
	defaultEtcdDataDirName        = "etcd-data"
	defaultSecretDirName          = "secrets"
	defaultNetworkDirName         = "net.d"
	defaultLogsDirName            = "logs"

	defaultKubectlClusterName = "p8s"
	defaultKubectlUserName    = "p8s"
	defaultKubectlContextName = "p8s"
)

// Environment is the main structure to configure the project
type Environment struct {
	// host
	rootABSPath string

	binABSPath string

	manifestTemplatesABSPath string
	manifestAPIABSPath       string
	manifestSystemdUnit      string
	manifestStaticPodABSPath string
	manifestConfigABSPath    string
	secretsABSPath           string
	networkConfigABSPath     string
	networkStateABSPath      string
	logsABSPath              string

	kubeletRootDir string

	kubeConfigUserPath     string
	kubeConfigAuthPath     string
	kubeConfigInsecurePath string
	etcdDataABSPath        string

	cleanOptions *options.Clean
	drainOptions *options.Drain

	hostname string

	// Systemd
	dbusClient        *dbus.Conn
	systemdUnitPrefix string

	containerRuntimeUnitName string
	etcdUnitName             string
	kubeletUnitName          string
	kubeAPIServerUnitName    string
	systemdUnitNames         []string

	// executable dependencies
	binaryHyperkube  *exeBinary
	binaryVault      *exeBinary
	binaryEtcd       *exeBinary
	binaryContainerd *exeBinary
	binaryCrio       *exeBinary
	binaryRunc       *exeBinary

	// dependencies
	downloadTimeout time.Duration
	binaryCNI       *depBinary

	templateMetadata *templateMetadata

	// Kubernetes Major.Minor
	templateVersion string

	systemdEnd2EndSection []*unit.UnitOption

	// Kubernetes apiserver
	restConfig *rest.Config
	clientSet  *kubernetes.Clientset

	// Kubernetes kubelet
	kubeletClient  *http.Client
	podListRequest *http.Request

	// Network
	outboundIP            *net.IP
	nodeIP                string
	kubernetesClusterCIDR *net.IPNet
	kubernetesClusterIP   *net.IP
	podCIDR               *net.IPNet
	podBridgeGatewayIP    *net.IP
	dnsClusterIP          *net.IP
	isDockerBridge        bool

	// Vault token
	vaultRootToken string

	kubectlLink string

	// CRI
	containerRuntimeInterface string
}

type templateMetadata struct {
	// pointers are used when fields are initialized later
	HyperkubeImageURL        string  `json:"hyperkube-image-url"`
	Hostname                 *string `json:"hostname"`
	RootABSPath              *string `json:"root"`
	ServiceClusterIPRange    string  `json:"service-cluster-ip-range"`
	KubernetesClusterIP      string  `json:"kubernetes-cluster-ip"`
	DNSClusterIP             string  `json:"dns-cluster-ip"`
	NodeIP                   *string `json:"node-ip"`
	KubeletRootDirABSPath    string  `json:"kubelet-root-dir"`
	CgroupDriver             string  `json:"cgroup-driver"`
	ContainerRuntime         string  `json:"container-runtime"`
	ContainerRuntimeEndpoint string  `json:"container-runtime-endpoint"`
}

// NewConfigSetup creates an Environment
// TODO this should be refactored with the viper migration
// TODO see https://github.com/DataDog/pupernetes/issues/40
func NewConfigSetup(givenRootPath string) (*Environment, error) {
	if givenRootPath == "" {
		err := fmt.Errorf("must provide a path")
		glog.Errorf("%v", err)
		return nil, err
	}
	rootABSPath, err := filepath.Abs(givenRootPath)
	if err != nil {
		glog.Errorf("Unexpected error during abspath: %v", err)
		return nil, err
	}

	e := &Environment{
		rootABSPath: rootABSPath,
		binABSPath:  path.Join(rootABSPath, defaultBinaryDirName),

		manifestTemplatesABSPath: path.Join(rootABSPath, defaultSourceTemplatesDirName),
		manifestStaticPodABSPath: path.Join(rootABSPath, defaultTemplates.ManifestStaticPod),
		manifestAPIABSPath:       path.Join(rootABSPath, defaultTemplates.ManifestAPI),
		manifestConfigABSPath:    path.Join(rootABSPath, defaultTemplates.ManifestConfig),
		manifestSystemdUnit:      path.Join(rootABSPath, defaultTemplates.ManifestSystemdUnit),
		kubeletRootDir:           config.ViperConfig.GetString("kubelet-root-dir"),
		secretsABSPath:           path.Join(rootABSPath, defaultSecretDirName),
		networkConfigABSPath:     path.Join(rootABSPath, defaultNetworkDirName),
		networkStateABSPath:      path.Join(rootABSPath, "networks"),
		logsABSPath:              path.Join(rootABSPath, defaultLogsDirName),
		templateVersion:          getMajorMinorVersion(config.ViperConfig.GetString("hyperkube-version")),

		kubeConfigUserPath:     config.ViperConfig.GetString("kubeconfig-path"),
		kubeConfigAuthPath:     path.Join(rootABSPath, defaultTemplates.ManifestConfig, "kubeconfig-auth.yaml"),
		kubeConfigInsecurePath: path.Join(rootABSPath, defaultTemplates.ManifestConfig, "kubeconfig-insecure.yaml"),
		etcdDataABSPath:        path.Join(rootABSPath, defaultEtcdDataDirName),
		cleanOptions:           options.NewCleanOptions(config.ViperConfig.GetString("clean"), config.ViperConfig.GetString("keep")),
		drainOptions:           options.NewDrainOptions(config.ViperConfig.GetString("drain")),
		kubectlLink:            config.ViperConfig.GetString("kubectl-link"),

		downloadTimeout: config.ViperConfig.GetDuration("download-timeout"),

		systemdUnitPrefix:         config.ViperConfig.GetString("systemd-unit-prefix"),
		etcdUnitName:              config.ViperConfig.GetString("systemd-unit-prefix") + "etcd.service",
		kubeletUnitName:           config.ViperConfig.GetString("systemd-unit-prefix") + "kubelet.service",
		kubeAPIServerUnitName:     config.ViperConfig.GetString("systemd-unit-prefix") + "kube-apiserver.service",
		containerRuntimeInterface: config.ViperConfig.GetString("container-runtime"),
	}
	// Kubernetes
	e.binaryHyperkube = &exeBinary{
		depBinary: depBinary{
			archivePath:     path.Join(e.binABSPath, fmt.Sprintf("hyperkube-v%s.tar.gz", config.ViperConfig.GetString("hyperkube-version"))),
			binaryABSPath:   path.Join(e.binABSPath, "hyperkube"),
			archiveURL:      fmt.Sprintf("https://dl.k8s.io/v%s/kubernetes-server-linux-amd64.tar.gz", config.ViperConfig.GetString("hyperkube-version")),
			version:         config.ViperConfig.GetString("hyperkube-version"),
			downloadTimeout: e.downloadTimeout,
		},
		skipVersionVerify: config.ViperConfig.GetBool("skip-binaries-version"),
		commandVersion:    []string{"kubelet", "--version"},
	}

	// Vault
	e.binaryVault = &exeBinary{
		depBinary: depBinary{
			archivePath:     path.Join(e.binABSPath, fmt.Sprintf("vault-v%s.zip", config.ViperConfig.GetString("vault-version"))),
			binaryABSPath:   path.Join(e.binABSPath, "vault"),
			archiveURL:      fmt.Sprintf("https://releases.hashicorp.com/vault/%s/vault_%s_linux_amd64.zip", config.ViperConfig.GetString("vault-version"), config.ViperConfig.GetString("vault-version")),
			version:         config.ViperConfig.GetString("vault-version"),
			downloadTimeout: e.downloadTimeout,
		},
		skipVersionVerify: config.ViperConfig.GetBool("skip-binaries-version"),
		commandVersion:    []string{"--version"},
	}

	// Etcd
	e.binaryEtcd = &exeBinary{
		depBinary: depBinary{
			archivePath:     path.Join(e.binABSPath, fmt.Sprintf("etcd-v%s.tar.gz", config.ViperConfig.GetString("etcd-version"))),
			binaryABSPath:   path.Join(e.binABSPath, "etcd"),
			archiveURL:      fmt.Sprintf("https://github.com/etcd-io/etcd/releases/download/v%s/etcd-v%s-linux-amd64.tar.gz", config.ViperConfig.GetString("etcd-version"), config.ViperConfig.GetString("etcd-version")),
			version:         config.ViperConfig.GetString("etcd-version"),
			downloadTimeout: e.downloadTimeout,
		},
		skipVersionVerify: config.ViperConfig.GetBool("skip-binaries-version"),
		commandVersion:    []string{"--version"},
	}

	// Containerd
	e.binaryContainerd = &exeBinary{
		depBinary: depBinary{
			archivePath:     path.Join(e.binABSPath, fmt.Sprintf("containerd-v%s.tar.gz", config.ViperConfig.GetString("containerd-version"))),
			binaryABSPath:   path.Join(e.binABSPath, "containerd"),
			archiveURL:      fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s.linux-amd64.tar.gz", config.ViperConfig.GetString("containerd-version"), config.ViperConfig.GetString("containerd-version")),
			version:         config.ViperConfig.GetString("containerd-version"),
			downloadTimeout: e.downloadTimeout,
		},
		skipVersionVerify: config.ViperConfig.GetBool("skip-binaries-version"),
		commandVersion:    []string{"--version"},
	}

	// CRI-o
	e.binaryCrio = &exeBinary{
		depBinary: depBinary{
			archivePath:     path.Join(e.binABSPath, fmt.Sprintf("crio-v%s.deb", config.ViperConfig.GetString("crio-version"))),
			binaryABSPath:   path.Join(e.binABSPath, "crio"),
			archiveURL:      fmt.Sprintf("https://launchpad.net/~projectatomic/+archive/ubuntu/ppa/+files/cri-o-1.11-stable_%s-1~ubuntu18.04~ppa3_amd64.deb", config.ViperConfig.GetString("crio-version")),
			version:         config.ViperConfig.GetString("crio-version"),
			downloadTimeout: e.downloadTimeout,
		},
		commandVersion: []string{"--version"},
	}

	// Runc
	e.binaryRunc = &exeBinary{
		depBinary: depBinary{
			archivePath:     path.Join(e.binABSPath, fmt.Sprintf("runc-v%s", config.ViperConfig.GetString("runc-version"))),
			binaryABSPath:   path.Join(e.binABSPath, "runc"),
			archiveURL:      fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/v%s/runc.amd64", config.ViperConfig.GetString("runc-version")),
			version:         config.ViperConfig.GetString("runc-version"),
			downloadTimeout: e.downloadTimeout,
		},
		skipVersionVerify: config.ViperConfig.GetBool("skip-binaries-version"),
		commandVersion:    []string{"--version"},
	}

	// CNI
	e.binaryCNI = &depBinary{
		archivePath:     path.Join(e.binABSPath, fmt.Sprintf("cni-v%s.tar.gz", config.ViperConfig.GetString("cni-version"))),
		binaryABSPath:   path.Join(e.binABSPath, "bridge"),
		archiveURL:      fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-amd64-v%s.tgz", config.ViperConfig.GetString("cni-version"), config.ViperConfig.GetString("cni-version")),
		version:         config.ViperConfig.GetString("cni-version"),
		downloadTimeout: e.downloadTimeout,
	}

	// SystemdUnits X-Section
	e.systemdEnd2EndSection = e.createEnd2EndSection()

	// Network
	_, e.kubernetesClusterCIDR, err = net.ParseCIDR(config.ViperConfig.GetString("kubernetes-cluster-ip-range"))
	if err != nil {
		glog.Errorf("Unexpected error while parsing kubernetes cluster IP range: %v", err)
		return nil, err
	}
	e.kubernetesClusterIP, err = pickInCIDR(e.kubernetesClusterCIDR.String(), 1)
	if err != nil {
		glog.Errorf("Cannot get Kubernetes cluster IP: %v", err)
		return nil, err
	}
	e.dnsClusterIP, err = pickInCIDR(e.kubernetesClusterCIDR.String(), 2)
	if err != nil {
		glog.Errorf("Cannot get DNS cluster IP: %v", err)
		return nil, err
	}
	_, e.podCIDR, err = net.ParseCIDR(config.ViperConfig.GetString("pod-ip-range"))
	if err != nil {
		glog.Errorf("Unexpected error while parsing pod IP range: %v", err)
		return nil, err
	}
	e.podBridgeGatewayIP, err = pickInCIDR(e.podCIDR.String(), 1)
	if err != nil {
		glog.Errorf("Cannot get pod gateway IP: %v", err)
		return nil, err
	}

	// kubeconfig
	if e.kubeConfigUserPath == "" {
		e.kubeConfigUserPath = path.Join(getHome(), ".kube", "config")
	}

	containerRuntime := "docker"
	ContainerRuntimeEndpoint := "/var/run/dockershim.sock"
	if e.containerRuntimeInterface == config.CRIContainerd {
		containerRuntime = "remote"
		ContainerRuntimeEndpoint = "/run/containerd/containerd.sock"
		e.systemdUnitNames = append(e.systemdUnitNames, fmt.Sprintf("%s%s.service", e.systemdUnitPrefix, e.containerRuntimeInterface))
	}
	if e.containerRuntimeInterface == config.CRICrio {
		containerRuntime = "remote"
		ContainerRuntimeEndpoint = "/run/crio/crio.sock"
		e.systemdUnitNames = append(e.systemdUnitNames, fmt.Sprintf("%s%s.service", e.systemdUnitPrefix, e.containerRuntimeInterface))
	}
	e.systemdUnitNames = append(e.systemdUnitNames, e.etcdUnitName, e.kubeAPIServerUnitName, e.kubeletUnitName)

	cgroupDriver := "cgroupfs"
	if containerRuntime == "docker" {
		c, err := client.NewEnvClient()
		if err != nil {
			glog.Warningf("Failed to guess docker cgroup driver, falling back to default '%s': %v", err, cgroupDriver)
		}
		info, err := c.Info(context.TODO())
		if err != nil {
			glog.Warningf("Failed to guess docker cgroup driver, falling back to default '%s': %v", err, cgroupDriver)
		}
		cgroupDriver = info.CgroupDriver
	}

	// Template for manifests
	e.templateMetadata = &templateMetadata{
		// TODO conf this
		HyperkubeImageURL:        fmt.Sprintf("gcr.io/google_containers/hyperkube:v%s", e.binaryHyperkube.version),
		Hostname:                 &e.hostname,
		RootABSPath:              &e.rootABSPath,
		ServiceClusterIPRange:    e.kubernetesClusterCIDR.String(),
		KubernetesClusterIP:      e.kubernetesClusterIP.String(),
		DNSClusterIP:             e.dnsClusterIP.String(),
		KubeletRootDirABSPath:    e.kubeletRootDir,
		ContainerRuntime:         containerRuntime,
		ContainerRuntimeEndpoint: ContainerRuntimeEndpoint,
		CgroupDriver:             cgroupDriver,
		NodeIP:                   &e.nodeIP, // initialized later
	}

	// Vault root token
	e.vaultRootToken = config.ViperConfig.GetString("vault-root-token")
	if e.vaultRootToken == "" {
		e.vaultRootToken = util.RandStringBytesMaskImprSrc(20)
		glog.V(4).Infof("Generated the vault root-token of length: %d", len(e.vaultRootToken))
	}
	return e, nil
}

func (e *Environment) setupDirectories() error {
	for _, dir := range []string{
		e.binABSPath,
		e.manifestTemplatesABSPath,
		e.manifestStaticPodABSPath,
		e.manifestConfigABSPath,
		e.manifestSystemdUnit,
		path.Join(e.manifestTemplatesABSPath, defaultTemplates.ManifestSystemdUnit),
		path.Join(e.manifestTemplatesABSPath, defaultTemplates.ManifestStaticPod),
		e.manifestAPIABSPath,
		path.Join(e.manifestTemplatesABSPath, defaultTemplates.ManifestAPI),
		path.Join(e.manifestTemplatesABSPath, defaultTemplates.ManifestConfig),
		e.etcdDataABSPath,
		e.secretsABSPath,
		e.networkConfigABSPath,
		e.kubeletRootDir,
		KubeletCRILogPath,
		e.logsABSPath,
	} {
		glog.V(4).Infof("Creating directory: %s", dir)
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			glog.Errorf("Cannot create %s: %v", dir, err)
			return err
		}
		glog.V(4).Infof("Directory exists: %s", dir)
	}
	return nil
}

// Setup the Environment
func (e *Environment) Setup() error {
	var err error
	glog.V(3).Infof("Setup starting %s", e.rootABSPath)
	for _, f := range []func() error{
		requirements.CheckRequirements,
		e.setupHostname,
		e.setupDirectories,
		e.setupBinaryCNI,
		e.setupBinaryEtcd,
		e.setupBinaryContainerd,
		e.setupBinaryCrio,
		e.setupBinaryRunc,
		e.setupBinaryVault,
		e.setupBinaryHyperkube,
		e.setupNetwork,
		e.setupManifests,
		e.setupSystemd,
		e.setupSecrets,
		e.setupKubeClients,
	} {
		err = f()
		if err != nil {
			return err
		}
	}
	glog.V(2).Infof("Setup ready %s", e.rootABSPath)
	return nil
}
