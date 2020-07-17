package machine

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"time"

	"github.com/code-ready/crc/pkg/crc/cluster"
	"github.com/code-ready/crc/pkg/crc/constants"
	"github.com/code-ready/crc/pkg/crc/errors"
	"github.com/code-ready/crc/pkg/crc/logging"
	"github.com/code-ready/crc/pkg/crc/network"
	crcssh "github.com/code-ready/crc/pkg/crc/ssh"
	"github.com/code-ready/crc/pkg/crc/systemd"
	crcos "github.com/code-ready/crc/pkg/os"

	// cluster services
	"github.com/code-ready/crc/pkg/crc/oc"
	"github.com/code-ready/crc/pkg/crc/services"
	"github.com/code-ready/crc/pkg/crc/services/dns"

	// machine related imports
	"github.com/code-ready/crc/pkg/crc/machine/bundle"
	"github.com/code-ready/crc/pkg/crc/machine/config"

	"github.com/code-ready/machine/libmachine"
	"github.com/code-ready/machine/libmachine/drivers"
	"github.com/code-ready/machine/libmachine/host"
	"github.com/code-ready/machine/libmachine/log"
	"github.com/code-ready/machine/libmachine/ssh"
	"github.com/code-ready/machine/libmachine/state"
)

func init() {
	// Force using the golang SSH implementation for windows
	if runtime.GOOS == crcos.WINDOWS.String() {
		ssh.SetDefaultClient(ssh.Native)
	}
}

func getClusterConfig(bundleInfo *bundle.CrcBundleInfo) (*ClusterConfig, error) {
	kubeadminPassword, err := bundleInfo.GetKubeadminPassword()
	if err != nil {
		return nil, fmt.Errorf("Error reading kubeadmin password from bundle %v", err)
	}
	proxyConfig, err := getProxyConfig(bundleInfo.ClusterInfo.BaseDomain)
	if err != nil {
		return nil, err
	}
	return &ClusterConfig{
		KubeConfig:    bundleInfo.GetKubeConfigPath(),
		KubeAdminPass: kubeadminPassword,
		WebConsoleURL: constants.DefaultWebConsoleURL,
		ClusterAPI:    constants.DefaultAPIURL,
		ProxyConfig:   proxyConfig,
	}, nil
}

func getCrcBundleInfo(bundlePath string) (*bundle.CrcBundleInfo, error) {
	bundleName := filepath.Base(bundlePath)
	bundleInfo, err := bundle.GetCachedBundleInfo(bundleName)
	if err == nil {
		logging.Infof("Loading bundle: %s ...", bundleName)
		return bundleInfo, nil
	}
	logging.Infof("Extracting bundle: %s ...", bundleName)
	return bundle.Extract(bundlePath)
}

func getBundleMetadataFromDriver(driver drivers.Driver) (string, *bundle.CrcBundleInfo, error) {
	bundleName, err := driver.GetBundleName()
	/* FIXME: the bundleName == "" check can be removed when all machine
	* drivers have been rebuilt with
	* https://github.com/code-ready/machine/commit/edeebfe54d1ca3f46c1c0bfb86846e54baf23708
	 */
	if bundleName == "" || err != nil {
		err := fmt.Errorf("Error getting bundle name from CodeReady Containers instance, make sure you ran 'crc setup' and are using the latest bundle")
		return "", nil, err
	}
	metadata, err := bundle.GetCachedBundleInfo(bundleName)
	if err != nil {
		return "", nil, err
	}

	return bundleName, metadata, err
}

func IsRunning(st state.State) bool {
	return st == state.Running
}

func createLibMachineClient(debug bool) (*libmachine.Client, func(), error) {
	err := setMachineLogging(debug)
	if err != nil {
		return nil, func() {
			unsetMachineLogging()
		}, err
	}
	client := libmachine.NewClient(constants.MachineBaseDir, constants.MachineCertsDir)
	return client, func() {
		client.Close()
		unsetMachineLogging()
	}, nil
}

func Start(startConfig StartConfig) (StartResult, error) {
	var crcBundleMetadata *bundle.CrcBundleInfo

	libMachineAPIClient, cleanup, err := createLibMachineClient(startConfig.Debug)
	defer cleanup()
	if err != nil {
		return startError(startConfig.Name, "Cannot initialize libmachine", err)
	}

	// Pre-VM start
	var privateKeyPath string
	var pullSecret string
	driverInfo := DefaultDriver
	exists, err := Exists(startConfig.Name)
	if !exists {
		machineConfig := config.MachineConfig{
			Name:       startConfig.Name,
			BundleName: filepath.Base(startConfig.BundlePath),
			VMDriver:   driverInfo.Driver,
			CPUs:       startConfig.CPUs,
			Memory:     startConfig.Memory,
		}

		pullSecret, err = startConfig.GetPullSecret()
		if err != nil {
			return startError(startConfig.Name, "Failed to get pull secret", err)
		}

		crcBundleMetadata, err = getCrcBundleInfo(startConfig.BundlePath)
		if err != nil {
			return startError(startConfig.Name, "Error getting bundle metadata", err)
		}

		logging.Infof("Checking size of the disk image %s ...", crcBundleMetadata.GetDiskImagePath())
		if err := crcBundleMetadata.CheckDiskImageSize(); err != nil {
			return startError(startConfig.Name, fmt.Sprintf("Invalid bundle disk image '%s'", crcBundleMetadata.GetDiskImagePath()), err)
		}

		openshiftVersion := crcBundleMetadata.GetOpenshiftVersion()
		if openshiftVersion == "" {
			logging.Info("Creating VM...")
		} else {
			logging.Infof("Creating CodeReady Containers VM for OpenShift %s...", openshiftVersion)
		}

		// Retrieve metadata info
		diskPath := crcBundleMetadata.GetDiskImagePath()
		machineConfig.DiskPathURL = fmt.Sprintf("file://%s", filepath.ToSlash(diskPath))
		machineConfig.SSHKeyPath = crcBundleMetadata.GetSSHKeyPath()
		machineConfig.KernelCmdLine = crcBundleMetadata.Nodes[0].KernelCmdLine
		machineConfig.Initramfs = crcBundleMetadata.GetInitramfsPath()
		machineConfig.Kernel = crcBundleMetadata.GetKernelPath()

		_, err := createHost(libMachineAPIClient, driverInfo.DriverPath, machineConfig)
		if err != nil {
			return startError(startConfig.Name, "Error creating machine", err)
		}

		privateKeyPath = crcBundleMetadata.GetSSHKeyPath()
	} else {
		host, err := libMachineAPIClient.Load(startConfig.Name)
		if err != nil {
			return startError(startConfig.Name, "Error loading machine", err)
		}

		var bundleName string
		bundleName, crcBundleMetadata, err = getBundleMetadataFromDriver(host.Driver)
		if err != nil {
			return startError(startConfig.Name, "Error loading bundle metadata", err)
		}
		if bundleName != filepath.Base(startConfig.BundlePath) {
			logging.Debugf("Bundle '%s' was requested, but the existing VM is using '%s'",
				filepath.Base(startConfig.BundlePath), bundleName)
			return startError(
				startConfig.Name,
				"Invalid bundle",
				fmt.Errorf("Bundle '%s' was requested, but the existing VM is using '%s'",
					filepath.Base(startConfig.BundlePath),
					bundleName))
		}
		vmState, err := host.Driver.GetState()
		if err != nil {
			return startError(startConfig.Name, "Error getting the machine state", err)
		}
		if IsRunning(vmState) {
			openshiftVersion := crcBundleMetadata.GetOpenshiftVersion()
			if openshiftVersion == "" {
				logging.Info("A CodeReady Containers VM is already running")
			} else {
				logging.Infof("A CodeReady Containers VM for OpenShift %s is already running", openshiftVersion)
			}
			return StartResult{
				Name:   startConfig.Name,
				Status: vmState.String(),
			}, nil
		}

		openshiftVersion := crcBundleMetadata.GetOpenshiftVersion()
		if openshiftVersion == "" {
			logging.Info("Starting CodeReady Containers VM ...")
		} else {
			logging.Infof("Starting CodeReady Containers VM for OpenShift %s...", openshiftVersion)
		}
		if err := host.Driver.Start(); err != nil {
			return startError(startConfig.Name, "Error starting stopped VM", err)
		}
		if err := libMachineAPIClient.Save(host); err != nil {
			return startError(startConfig.Name, "Error saving state for VM", err)
		}

		privateKeyPath = constants.GetPrivateKeyPath()
	}

	clusterConfig, err := getClusterConfig(crcBundleMetadata)
	if err != nil {
		return startError(startConfig.Name, "Cannot create cluster configuration", err)
	}

	// Post-VM start
	host, err := libMachineAPIClient.Load(startConfig.Name)
	if err != nil {
		return startError(startConfig.Name, fmt.Sprintf("Error loading %s vm", startConfig.Name), err)
	}

	vmState, err := host.Driver.GetState()
	if err != nil {
		return startError(startConfig.Name, "Error getting the state", err)
	}

	sshRunner := crcssh.CreateRunnerWithPrivateKey(host.Driver, privateKeyPath)

	logging.Debug("Waiting until ssh is available")
	if err := cluster.WaitForSSH(sshRunner); err != nil {
		return startError(startConfig.Name, "Failed to connect to the CRC VM with SSH -- host might be unreachable", err)
	}
	logging.Info("CodeReady Containers VM is running")

	// Check the certs validity inside the vm
	needsCertsRenewal := false
	logging.Info("Verifying validity of the cluster certificates ...")
	certExpiryState, err := cluster.CheckCertsValidity(sshRunner)
	if err != nil {
		if certExpiryState == cluster.CertExpired {
			needsCertsRenewal = true
		} else {
			return startError(startConfig.Name, "Failed to check certificate validity", err)
		}
	}
	// Add nameserver to VM if provided by User
	if startConfig.NameServer != "" {
		if err = addNameServerToInstance(sshRunner, startConfig.NameServer); err != nil {
			return startError(startConfig.Name, "Failed to add nameserver to the VM", err)
		}
	}

	instanceIP, err := host.Driver.GetIP()
	if err != nil {
		return startError(startConfig.Name, "Error getting the IP", err)
	}

	var hostIP string
	determineHostIP := func() error {
		hostIP, err = network.DetermineHostIP(instanceIP)
		if err != nil {
			logging.Debugf("Error finding host IP (%v) - retrying", err)
			return &errors.RetriableError{Err: err}
		}
		return nil
	}

	if err := errors.RetryAfter(30, determineHostIP, 2*time.Second); err != nil {
		return startError(startConfig.Name, "Error determining host IP", err)
	}

	proxyConfig, err := getProxyConfig(crcBundleMetadata.ClusterInfo.BaseDomain)
	if err != nil {
		return startError(startConfig.Name, "Error getting proxy configuration", err)
	}
	proxyConfig.ApplyToEnvironment()

	// Create servicePostStartConfig for DNS checks and DNS start.
	servicePostStartConfig := services.ServicePostStartConfig{
		Name:       startConfig.Name,
		DriverName: host.Driver.DriverName(),
		// TODO: would prefer passing in a more generic type
		SSHRunner: sshRunner,
		IP:        instanceIP,
		HostIP:    hostIP,
		// TODO: should be more finegrained
		BundleMetadata: *crcBundleMetadata,
	}

	// Run the DNS server inside the VM
	if _, err := dns.RunPostStart(servicePostStartConfig); err != nil {
		return startError(startConfig.Name, "Error running post start", err)
	}

	// Check DNS lookup before starting the kubelet
	if queryOutput, err := dns.CheckCRCLocalDNSReachable(servicePostStartConfig); err != nil {
		return startError(startConfig.Name, fmt.Sprintf("Failed internal DNS query: %s", queryOutput), err)
	}
	logging.Info("Check internal and public DNS query ...")

	if queryOutput, err := dns.CheckCRCPublicDNSReachable(servicePostStartConfig); err != nil {
		logging.Warnf("Failed public DNS query from the cluster: %v : %s", err, queryOutput)
	}

	// Check DNS lookup from host to VM
	logging.Info("Check DNS query from host ...")
	if err := network.CheckCRCLocalDNSReachableFromHost(crcBundleMetadata, instanceIP); err != nil {
		return startError(startConfig.Name, "Failed to query DNS from host", err)
	}

	// Additional steps to perform after newly created VM is up
	if !exists {
		logging.Info("Generating new SSH key")
		if err := updateSSHKeyPair(sshRunner); err != nil {

			return startError(startConfig.Name, "Error updating public key", err)
		}
		// Copy Kubeconfig file from bundle extract path to machine directory.
		// In our case it would be ~/.crc/machines/crc/
		logging.Info("Copying kubeconfig file to instance dir ...")
		kubeConfigFilePath := filepath.Join(constants.MachineInstanceDir, startConfig.Name, "kubeconfig")
		err := crcos.CopyFileContents(crcBundleMetadata.GetKubeConfigPath(),
			kubeConfigFilePath,
			0644)
		if err != nil {
			return startError(startConfig.Name, "Error copying kubeconfig file", err)
		}
		// Copy kubeconfig file inside the VM
		kubeconfigContent, _ := ioutil.ReadFile(kubeConfigFilePath)
		_, err = sshRunner.RunPrivate(fmt.Sprintf("cat <<EOF | sudo tee /opt/kubeconfig\n%s\nEOF", string(kubeconfigContent)))
		if err != nil {
			return startError(startConfig.Name, "Error copying kubeconfig file in VM", err)
		}
	}

	ocConfig := oc.UseOCWithSSH(sshRunner)
	if needsCertsRenewal {
		logging.Info("Cluster TLS certificates have expired, renewing them... [will take up to 5 minutes]")
		err = cluster.RegenerateCertificates(sshRunner, ocConfig)
		if err != nil {
			logging.Debugf("Failed to renew TLS certificates: %v", err)
			buildTime, getBuildTimeErr := crcBundleMetadata.GetBundleBuildTime()
			if getBuildTimeErr == nil {
				bundleAgeDays := time.Since(buildTime).Hours() / 24
				if bundleAgeDays >= 30 {
					/* Initial bundle certificates are only valid for 30 days */
					logging.Debugf("Bundle has been generated %d days ago", int(bundleAgeDays))
				}
			}
			return startError(startConfig.Name, "Failed to renew TLS certificates: please check if a newer CodeReady Containers release is available", err)
		}
	}

	logging.Info("Starting OpenShift kubelet service")
	sd := systemd.NewInstanceSystemdCommander(sshRunner)
	if _, err := sd.Start("kubelet"); err != nil {
		return startError(startConfig.Name, "Error starting kubelet", err)
	}
	if !exists {
		logging.Info("Configuring cluster for first start")
		if err := configureCluster(ocConfig, sshRunner, proxyConfig, pullSecret, instanceIP); err != nil {
			return startError(startConfig.Name, "Error Setting cluster config", err)
		}
	}

	// Check if kubelet service is running inside the VM
	kubeletStarted, err := sd.IsActive("kubelet")
	if err != nil {
		return startError(startConfig.Name, "kubelet service is not running", err)
	}
	if kubeletStarted {
		// In Openshift 4.3, when cluster comes up, the following happens
		// 1. After the openshift-apiserver pod is started, its log contains multiple occurrences of `certificate has expired or is not yet valid`
		// 2. Initially there is no request-header's client-ca crt available to `extension-apiserver-authentication` configmap
		// 3. In the pod logs `missing content for CA bundle "client-ca::kube-system::extension-apiserver-authentication::requestheader-client-ca-file"`
		// 4. After ~1 min /etc/kubernetes/static-pod-resources/kube-apiserver-certs/configmaps/aggregator-client-ca/ca-bundle.crt is regenerated
		// 5. It is now also appear to `extension-apiserver-authentication` configmap as part of request-header's client-ca content
		// 6. Openshift-apiserver is able to load the CA which was regenerated
		// 7. Now apiserver pod log contains multiple occurrences of `error x509: certificate signed by unknown authority`
		// When the openshift-apiserver is in this state, the cluster is non functional.
		// A restart of the openshift-apiserver pod is enough to clear that error and get a working cluster.
		// This is a work-around while the root cause is being identified.
		// More info: https://bugzilla.redhat.com/show_bug.cgi?id=1795163
		logging.Debug("Waiting for update of client-ca request header ...")
		if err := cluster.WaitforRequestHeaderClientCaFile(ocConfig); err != nil {
			return startError(startConfig.Name, "Failed to wait for the client-ca request header update", err)
		}

		if err := cluster.DeleteOpenshiftAPIServerPods(ocConfig); err != nil {
			return startError(startConfig.Name, "Cannot delete OpenShift API Server pods", err)
		}

		logging.Info("Starting OpenShift cluster ... [waiting 3m]")
	}

	time.Sleep(time.Minute * 3)

	// Approve the node certificate.
	if err := cluster.ApproveNodeCSR(ocConfig); err != nil {
		return startError(startConfig.Name, "Error approving the node csr", err)
	}

	if proxyConfig.IsEnabled() {
		logging.Info("Waiting for the proxy configuration to be applied ...")
		waitForProxyPropagation(ocConfig, proxyConfig)
	}

	// If no error, return usage message
	logging.Info("")
	logging.Info("To access the cluster, first set up your environment by following 'crc oc-env' instructions")
	logging.Infof("Then you can access it by running 'oc login -u developer -p developer %s'", clusterConfig.ClusterAPI)
	logging.Infof("To login as an admin, run 'oc login -u kubeadmin -p %s %s'", clusterConfig.KubeAdminPass, clusterConfig.ClusterAPI)
	logging.Info("")
	logging.Info("You can now run 'crc console' and use these credentials to access the OpenShift web console")

	return StartResult{
		Name:           startConfig.Name,
		KubeletStarted: kubeletStarted,
		ClusterConfig:  *clusterConfig,
		Status:         vmState.String(),
	}, err
}

func Stop(stopConfig StopConfig) (StopResult, error) {
	defer unsetMachineLogging()

	// Set libmachine logging
	err := setMachineLogging(stopConfig.Debug)
	if err != nil {
		return stopError(stopConfig.Name, "Cannot initialize logging", err)
	}

	libMachineAPIClient, cleanup, err := createLibMachineClient(stopConfig.Debug)
	defer cleanup()
	if err != nil {
		return stopError(stopConfig.Name, "Cannot initialize libmachine", err)
	}
	host, err := libMachineAPIClient.Load(stopConfig.Name)

	if err != nil {
		return stopError(stopConfig.Name, "Cannot load machine", err)
	}

	state, _ := host.Driver.GetState()

	if err := host.Stop(); err != nil {
		return stopError(stopConfig.Name, "Cannot stop machine", err)
	}

	return StopResult{
		Name:    stopConfig.Name,
		Success: true,
		State:   state,
	}, nil
}

func PowerOff(powerOff PowerOffConfig) (PowerOffResult, error) {
	libMachineAPIClient, cleanup, err := createLibMachineClient(false)
	defer cleanup()
	if err != nil {
		return powerOffError(powerOff.Name, "Cannot initialize libmachine", err)
	}
	host, err := libMachineAPIClient.Load(powerOff.Name)

	if err != nil {
		return powerOffError(powerOff.Name, "Cannot load machine", err)
	}

	if err := host.Kill(); err != nil {
		return powerOffError(powerOff.Name, "Cannot kill machine", err)
	}

	return PowerOffResult{
		Name:    powerOff.Name,
		Success: true,
	}, nil
}

func Delete(deleteConfig DeleteConfig) (DeleteResult, error) {
	libMachineAPIClient, cleanup, err := createLibMachineClient(false)
	defer cleanup()
	if err != nil {
		return deleteError(deleteConfig.Name, "Cannot initialize libmachine", err)
	}
	host, err := libMachineAPIClient.Load(deleteConfig.Name)

	if err != nil {
		return deleteError(deleteConfig.Name, "Cannot load machine", err)
	}

	if err := host.Driver.Remove(); err != nil {
		return deleteError(deleteConfig.Name, "Driver cannot remove machine", err)
	}

	if err := libMachineAPIClient.Remove(deleteConfig.Name); err != nil {
		return deleteError(deleteConfig.Name, "Cannot remove machine", err)
	}

	return DeleteResult{
		Name:    deleteConfig.Name,
		Success: true,
	}, nil
}

func IP(ipConfig IPConfig) (IPResult, error) {
	err := setMachineLogging(ipConfig.Debug)
	if err != nil {
		return ipError(ipConfig.Name, "Cannot initialize logging", err)
	}

	libMachineAPIClient, cleanup, err := createLibMachineClient(ipConfig.Debug)
	defer cleanup()
	if err != nil {
		return ipError(ipConfig.Name, "Cannot initialize libmachine", err)
	}
	host, err := libMachineAPIClient.Load(ipConfig.Name)

	if err != nil {
		return ipError(ipConfig.Name, "Cannot load machine", err)
	}
	ip, err := host.Driver.GetIP()
	if err != nil {
		return ipError(ipConfig.Name, "Cannot get IP", err)
	}
	return IPResult{
		Name:    ipConfig.Name,
		Success: true,
		IP:      ip,
	}, nil
}

func Status(statusConfig ClusterStatusConfig) (ClusterStatusResult, error) {
	libMachineAPIClient, cleanup, err := createLibMachineClient(false)
	defer cleanup()
	if err != nil {
		return statusError(statusConfig.Name, "Cannot initialize libmachine", err)
	}

	_, err = libMachineAPIClient.Exists(statusConfig.Name)
	if err != nil {
		return statusError(statusConfig.Name, "Cannot check if machine exists", err)
	}

	openshiftStatus := "Stopped"
	var diskUse int64
	var diskSize int64

	host, err := libMachineAPIClient.Load(statusConfig.Name)
	if err != nil {
		return statusError(statusConfig.Name, "Cannot load machine", err)
	}
	vmStatus, err := host.Driver.GetState()
	if err != nil {
		return statusError(statusConfig.Name, "Cannot get machine state", err)
	}

	if IsRunning(vmStatus) {
		_, crcBundleMetadata, err := getBundleMetadataFromDriver(host.Driver)
		if err != nil {
			return statusError(statusConfig.Name, "Error loading bundle metadata", err)
		}
		proxyConfig, err := getProxyConfig(crcBundleMetadata.ClusterInfo.BaseDomain)
		if err != nil {
			return statusError(statusConfig.Name, "Error getting proxy configuration", err)
		}
		proxyConfig.ApplyToEnvironment()

		sshRunner := crcssh.CreateRunner(host.Driver)
		// check if all the clusteroperators are running
		ocConfig := oc.UseOCWithSSH(sshRunner)
		operatorsStatus, err := cluster.GetClusterOperatorsStatus(ocConfig)
		if err != nil {
			openshiftStatus = "Not Reachable"
			logging.Debug(err.Error())
		}
		switch {
		case operatorsStatus.Available:
			openshiftVersion := "4.x"
			if crcBundleMetadata.GetOpenshiftVersion() != "" {
				openshiftVersion = crcBundleMetadata.GetOpenshiftVersion()
			}
			openshiftStatus = fmt.Sprintf("Running (v%s)", openshiftVersion)
		case operatorsStatus.Degraded:
			openshiftStatus = "Degraded"
		case operatorsStatus.Progressing:
			openshiftStatus = "Starting"
		}
		diskSize, diskUse, err = cluster.GetRootPartitionUsage(sshRunner)
		if err != nil {
			return statusError(statusConfig.Name, "Cannot get root partition usage", err)
		}
	}
	return ClusterStatusResult{
		Name:            statusConfig.Name,
		CrcStatus:       vmStatus.String(),
		OpenshiftStatus: openshiftStatus,
		DiskUse:         diskUse,
		DiskSize:        diskSize,
		Success:         true,
	}, nil
}

func Exists(name string) (bool, error) {
	libMachineAPIClient, cleanup, err := createLibMachineClient(false)
	defer cleanup()
	if err != nil {
		return false, err
	}
	exists, err := libMachineAPIClient.Exists(name)
	if err != nil {
		return false, fmt.Errorf("Error checking if the host exists: %s", err)
	}
	return exists, nil
}

func createHost(api libmachine.API, driverPath string, machineConfig config.MachineConfig) (*host.Host, error) {
	driverOptions := getDriverOptions(machineConfig)
	jsonDriverConfig, err := json.Marshal(driverOptions)
	if err != nil {
		return nil, errors.New("Failed to marshal driver options")
	}

	vm, err := api.NewHost(machineConfig.VMDriver, driverPath, jsonDriverConfig)

	if err != nil {
		return nil, fmt.Errorf("Error creating new host: %s", err)
	}

	if err := api.Create(vm); err != nil {
		return nil, fmt.Errorf("Error creating the VM: %s", err)
	}

	return vm, nil
}

func setMachineLogging(logs bool) error {
	if !logs {
		log.SetDebug(true)
		logfile, err := logging.OpenLogFile(constants.LogFilePath)
		if err != nil {
			return err
		}
		log.SetOutWriter(logfile)
		log.SetErrWriter(logfile)
	} else {
		log.SetDebug(true)
	}
	return nil
}

func unsetMachineLogging() {
	logging.CloseLogFile()
}

func addNameServerToInstance(sshRunner *crcssh.Runner, ns string) error {
	nameserver := network.NameServer{IPAddress: ns}
	nameservers := []network.NameServer{nameserver}
	exist, err := network.HasGivenNameserversConfigured(sshRunner, nameserver)
	if err != nil {
		return err
	}
	if !exist {
		logging.Infof("Adding %s as nameserver to the instance ...", nameserver.IPAddress)
		return network.AddNameserversToInstance(sshRunner, nameservers)
	}
	return nil
}

// Return proxy config if VM is present
func GetProxyConfig(machineName string) (*network.ProxyConfig, error) {
	// Here we are only checking if the VM exist and not the status of the VM.
	// We might need to improve and use crc status logic, only
	// return if the Openshift is running as part of status.
	libMachineAPIClient, cleanup, err := createLibMachineClient(false)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	host, err := libMachineAPIClient.Load(machineName)

	if err != nil {
		return nil, errors.New(err.Error())
	}

	_, crcBundleMetadata, err := getBundleMetadataFromDriver(host.Driver)
	if err != nil {
		return nil, errors.Newf("Error loading bundle metadata: %v", err)
	}

	clusterConfig, err := getClusterConfig(crcBundleMetadata)
	if err != nil {
		return nil, errors.Newf("Error loading cluster configuration: %v", err)
	}

	return clusterConfig.ProxyConfig, nil
}

// Return console URL if the VM is present.
func GetConsoleURL(consoleConfig ConsoleConfig) (ConsoleResult, error) {
	// Here we are only checking if the VM exist and not the status of the VM.
	// We might need to improve and use crc status logic, only
	// return if the Openshift is running as part of status.
	libMachineAPIClient, cleanup, err := createLibMachineClient(false)
	defer cleanup()
	if err != nil {
		return consoleURLError("Cannot initialize libmachine", err)
	}
	host, err := libMachineAPIClient.Load(consoleConfig.Name)
	if err != nil {
		return consoleURLError("Cannot load machine", err)
	}

	vmState, err := host.Driver.GetState()
	if err != nil {
		return consoleURLError("Error getting the state for host", err)
	}

	_, crcBundleMetadata, err := getBundleMetadataFromDriver(host.Driver)
	if err != nil {
		return consoleURLError("Error loading bundle metadata", err)
	}

	clusterConfig, err := getClusterConfig(crcBundleMetadata)
	if err != nil {
		return consoleURLError("Error loading cluster configuration", err)
	}

	return ConsoleResult{
		Success:       true,
		ClusterConfig: *clusterConfig,
		State:         vmState,
	}, nil
}

func updateSSHKeyPair(sshRunner *crcssh.Runner) error {
	// Generate ssh key pair
	if err := ssh.GenerateSSHKey(constants.GetPrivateKeyPath()); err != nil {
		return fmt.Errorf("Error generating ssh key pair: %v", err)
	}

	// Read generated public key
	publicKey, err := ioutil.ReadFile(constants.GetPublicKeyPath())
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("echo '%s' > /home/core/.ssh/authorized_keys", publicKey)
	_, err = sshRunner.Run(cmd)
	if err != nil {
		return err
	}
	sshRunner.SetPrivateKeyPath(constants.GetPrivateKeyPath())

	return err
}

func configureCluster(ocConfig oc.Config, sshRunner *crcssh.Runner, proxyConfig *network.ProxyConfig, pullSecret, instanceIP string) (rerr error) {
	sd := systemd.NewInstanceSystemdCommander(sshRunner)

	if err := configProxyForCluster(ocConfig, sshRunner, sd, proxyConfig, instanceIP); err != nil {
		return fmt.Errorf("Failed to configure proxy for cluster: %v", err)
	}

	logging.Info("Adding user's pull secret ...")
	if err := cluster.AddPullSecret(sshRunner, ocConfig, pullSecret); err != nil {
		return fmt.Errorf("Failed to update user pull secret or cluster ID: %v", err)
	}
	logging.Info("Updating cluster ID ...")
	if err := cluster.UpdateClusterID(ocConfig); err != nil {
		return fmt.Errorf("Failed to update cluster ID: %v", err)
	}

	return nil
}

func getProxyConfig(baseDomainName string) (*network.ProxyConfig, error) {
	proxy, err := network.NewProxyConfig()
	if err != nil {
		return nil, err
	}
	if proxy.IsEnabled() {
		proxy.AddNoProxy(fmt.Sprintf(".%s", baseDomainName))
	}

	return proxy, nil
}

func configProxyForCluster(ocConfig oc.Config, sshRunner *crcssh.Runner, sd *systemd.InstanceSystemdCommander,
	proxy *network.ProxyConfig, instanceIP string) (err error) {
	if !proxy.IsEnabled() {
		return nil
	}

	defer func() {
		// Restart the crio service
		if proxy.IsEnabled() {
			// Restart reload the daemon and then restart the service
			// So no need to explicit reload the daemon.
			if _, ferr := sd.Restart("crio"); ferr != nil {
				err = ferr
			}
			if _, ferr := sd.Restart("kubelet"); ferr != nil {
				err = ferr
			}
		}
	}()

	logging.Info("Adding proxy configuration to the cluster ...")
	proxy.AddNoProxy(instanceIP)
	if err := cluster.AddProxyConfigToCluster(ocConfig, proxy); err != nil {
		return err
	}

	logging.Info("Adding proxy configuration to kubelet and crio service ...")
	if err := cluster.AddProxyToKubeletAndCriO(sshRunner, proxy); err != nil {
		return err
	}

	return nil
}

func waitForProxyPropagation(ocConfig oc.Config, proxyConfig *network.ProxyConfig) {
	checkProxySettingsForOperator := func() error {
		proxySet, err := cluster.CheckProxySettingsForOperator(ocConfig, proxyConfig, "redhat-operators", "openshift-marketplace")
		if err != nil {
			logging.Debugf("Error getting proxy setting for openshift-marketplace operator %v", err)
			return &errors.RetriableError{Err: err}
		}
		if !proxySet {
			logging.Debug("Proxy changes for cluster in progress")
			return &errors.RetriableError{Err: fmt.Errorf("")}
		}
		return nil
	}

	if err := errors.RetryAfter(60, checkProxySettingsForOperator, 2*time.Second); err != nil {
		logging.Debug("Failed to propagate proxy settings to cluster")
	}
}
