package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	defaultRegion            = "us-central1"
	karpenterNamespace       = "karpenter-system"
	karpenterServiceAccount  = "karpenter"
	controllerStartupTimeout = 5 * time.Minute
	clusterCreateTimeout     = 25 * time.Minute
	clusterDeleteTimeout     = 10 * time.Minute
	computeDeleteTimeout     = 5 * time.Minute
	suiteCleanupTimeout      = 2 * time.Minute
	defaultNodeMachineType   = "e2-small"

	primarySubnetCIDR  = "10.0.0.0/20"
	podsSecondaryCIDR  = "10.4.0.0/14"
	servicesSecondCIDR = "10.8.0.0/20"
)

type cleanupStack struct {
	mu    sync.Mutex
	funcs []func() error
}

func (s *cleanupStack) push(f func() error) {
	if f == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.funcs = append(s.funcs, f)
}

func (s *cleanupStack) runAll() error {
	s.mu.Lock()
	funcs := append([]func() error(nil), s.funcs...)
	s.mu.Unlock()

	var errs []string
	for i := len(funcs) - 1; i >= 0; i-- {
		func(idx int, f func() error) {
			defer func() {
				if r := recover(); r != nil {
					errMsg := fmt.Sprintf("cleanup step %d panicked: %v", idx, r)
					fmt.Fprintln(os.Stderr, errMsg)
					errs = append(errs, errMsg)
				}
			}()
			if err := f(); err != nil {
				errMsg := fmt.Sprintf("cleanup step %d failed: %v", idx, err)
				fmt.Fprintln(os.Stderr, errMsg)
				errs = append(errs, errMsg)
			}
		}(i, funcs[i])
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

type suiteState struct {
	projectID          string
	credentialsPath    string
	prefix             string
	region             string
	zone               string
	repoRoot           string
	tempDir            string
	kubeconfigPath     string
	crdPath            string
	testNamespace      string
	clusterName        string
	networkName        string
	subnetworkName     string
	podsRangeName      string
	servicesRangeName  string
	karpenterGSAID     string
	karpenterGSAEmail  string
	karpenterKeyPath   string
	karpenterKSAMember string
	clientset          kubernetes.Interface
	dynamicClient      dynamic.Interface
	computeService     *compute.Service
	containerService   *container.Service
	controllerCmd      *exec.Cmd
	controllerDone     chan error
	controllerExitErr  error
	cleanup            cleanupStack
}

var testEnv suiteState

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) (exitCode int) {
	exitCode = 1
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\ne2e: received %s, running cleanup...\n", sig)
		if err := cleanupSuite(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e cleanup after signal failed: %v\n", err)
		}
		os.Exit(1)
	}()
	defer func() {
		signal.Stop(sigCh)
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "e2e suite panicked: %v\n", r)
		}
		if err := cleanupSuite(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e cleanup failed: %v\n", err)
			exitCode = 1
		}
	}()

	if err := setupSuite(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
		return 1
	}

	exitCode = m.Run()
	return exitCode
}

func setupSuite() error {
	repoRoot, err := repoRoot()
	if err != nil {
		return err
	}

	saPath := os.Getenv("E2E_SA_PATH")
	if saPath == "" {
		saPath = filepath.Join(repoRoot, "karpenter-e2e-key.json")
	}
	credentialsPath, err := filepath.Abs(saPath)
	if err != nil {
		return fmt.Errorf("resolving credentials path: %w", err)
	}
	if _, err := os.Stat(credentialsPath); err != nil {
		return fmt.Errorf("stat credentials path: %w", err)
	}

	projectID := strings.TrimSpace(os.Getenv("E2E_PROJECT_ID"))
	if projectID == "" {
		keyData, err := os.ReadFile(credentialsPath)
		if err != nil {
			return fmt.Errorf("reading credentials file to extract project_id: %w", err)
		}
		var keyFile struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.Unmarshal(keyData, &keyFile); err != nil {
			return fmt.Errorf("parsing credentials JSON: %w", err)
		}
		if keyFile.ProjectID == "" {
			return fmt.Errorf("E2E_PROJECT_ID not set and credentials file has no project_id field")
		}
		projectID = keyFile.ProjectID
		fmt.Printf("e2e: extracted project_id=%s from credentials file\n", projectID)
	}
	prefix := sanitizeName(os.Getenv("E2E_PREFIX"), 20)
	if prefix == "" {
		prefix = "karp-e2e"
	}
	tempDir, err := os.MkdirTemp("", prefix+"-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}

	region := defaultRegion
	zone := fmt.Sprintf("%s-a", region)
	kubeconfigPath := filepath.Join(tempDir, "kubeconfig")

	testEnv = suiteState{
		projectID:          projectID,
		credentialsPath:    credentialsPath,
		prefix:             prefix,
		region:             region,
		zone:               zone,
		repoRoot:           repoRoot,
		tempDir:            tempDir,
		kubeconfigPath:     kubeconfigPath,
		crdPath:            filepath.Join(repoRoot, "charts", "karpenter", "crds"),
		testNamespace:      gcpName(prefix, "test", 63),
		clusterName:        gcpName(prefix, "cluster", 40),
		networkName:        gcpName(prefix, "vpc", 63),
		subnetworkName:     gcpName(prefix, "subnet", 63),
		podsRangeName:      gcpName(prefix, "pods", 63),
		servicesRangeName:  gcpName(prefix, "services", 63),
		karpenterGSAID:     gcpName(prefix, "karpenter", 30),
		karpenterKSAMember: fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", projectID, karpenterNamespace, karpenterServiceAccount),
	}
	testEnv.karpenterGSAEmail = fmt.Sprintf("%s@%s.iam.gserviceaccount.com", testEnv.karpenterGSAID, projectID)
	testEnv.cleanup.push(func() error { return os.RemoveAll(tempDir) })

	if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentialsPath); err != nil {
		return fmt.Errorf("setting GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
	if err := os.Setenv("KUBECONFIG", kubeconfigPath); err != nil {
		return fmt.Errorf("setting KUBECONFIG: %w", err)
	}

	ctx := context.Background()
	testEnv.computeService, err = compute.NewService(ctx, option.WithCredentialsFile(credentialsPath))
	if err != nil {
		return fmt.Errorf("creating compute service: %w", err)
	}
	testEnv.containerService, err = container.NewService(ctx, option.WithCredentialsFile(credentialsPath))
	if err != nil {
		return fmt.Errorf("creating container service: %w", err)
	}

	if err := runCommand(repoRoot, cloudEnv(nil), "gcloud", "auth", "activate-service-account", "--key-file", credentialsPath, "--project", projectID, "--quiet"); err != nil {
		return err
	}

	if err := createNetwork(ctx); err != nil {
		return err
	}
	if err := createSubnetwork(ctx); err != nil {
		return err
	}
	if err := createKarpenterServiceAccount(); err != nil {
		return err
	}
	if err := createKarpenterSAKey(); err != nil {
		return err
	}
	if err := createCluster(ctx); err != nil {
		return err
	}
	if err := removeDefaultNodePools(ctx); err != nil {
		return err
	}
	if err := getClusterCredentials(); err != nil {
		return err
	}

	restConfig, err := loadRESTConfig()
	if err != nil {
		return err
	}
	testEnv.clientset, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}
	testEnv.dynamicClient, err = dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	if err := createNamespacesAndKSA(ctx); err != nil {
		return err
	}
	testEnv.cleanup.push(func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), suiteCleanupTimeout)
		defer cancel()
		return deleteSuiteResourcesFromCluster(cleanupCtx)
	})
	if err := addWorkloadIdentityBinding(); err != nil {
		return err
	}
	if err := runCommand(repoRoot, cloudEnv(nil), "kubectl", "apply", "-f", testEnv.crdPath); err != nil {
		return err
	}
	if err := startController(); err != nil {
		return err
	}
	if err := waitForControllerReady(ctx); err != nil {
		return err
	}

	return nil
}

func cleanupSuite() error {
	cleanupErr := testEnv.cleanup.runAll()
	verifyErr := verifyProjectClean(context.Background())
	if cleanupErr != nil && verifyErr != nil {
		return fmt.Errorf("%v; %v", cleanupErr, verifyErr)
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	return verifyErr
}

func createNetwork(ctx context.Context) error {
	op, err := testEnv.computeService.Networks.Insert(testEnv.projectID, &compute.Network{
		Name:                  testEnv.networkName,
		AutoCreateSubnetworks: false,
		Mtu:                   1460,
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("creating network %s: %w", testEnv.networkName, err)
	}
	testEnv.cleanup.push(func() error { return deleteNetwork(context.Background()) })
	if err := waitForGlobalComputeOperation(ctx, op.Name); err != nil {
		return fmt.Errorf("waiting for network create: %w", err)
	}
	return nil
}

func createSubnetwork(ctx context.Context) error {
	networkLink := fmt.Sprintf("projects/%s/global/networks/%s", testEnv.projectID, testEnv.networkName)
	op, err := testEnv.computeService.Subnetworks.Insert(testEnv.projectID, testEnv.region, &compute.Subnetwork{
		Name:        testEnv.subnetworkName,
		IpCidrRange: primarySubnetCIDR,
		Network:     networkLink,
		SecondaryIpRanges: []*compute.SubnetworkSecondaryRange{
			{RangeName: testEnv.podsRangeName, IpCidrRange: podsSecondaryCIDR},
			{RangeName: testEnv.servicesRangeName, IpCidrRange: servicesSecondCIDR},
		},
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("creating subnetwork %s: %w", testEnv.subnetworkName, err)
	}
	testEnv.cleanup.push(func() error { return deleteSubnetwork(context.Background()) })
	if err := waitForRegionalComputeOperation(ctx, testEnv.region, op.Name); err != nil {
		return fmt.Errorf("waiting for subnetwork create: %w", err)
	}
	return nil
}

func createKarpenterServiceAccount() error {
	if err := runCommand(testEnv.repoRoot, cloudEnv(nil), "gcloud", "iam", "service-accounts", "create", testEnv.karpenterGSAID,
		"--project", testEnv.projectID,
		"--display-name", "Karpenter e2e controller",
		"--quiet",
	); err != nil {
		return err
	}
	testEnv.cleanup.push(func() error { return deleteServiceAccount() })

	roles := []string{
		"roles/compute.admin",
		"roles/container.admin",
		"roles/iam.serviceAccountUser",
	}
	for _, role := range roles {
		if err := runCommand(testEnv.repoRoot, cloudEnv(nil), "gcloud", "projects", "add-iam-policy-binding", testEnv.projectID,
			"--member", "serviceAccount:"+testEnv.karpenterGSAEmail,
			"--role", role,
			"--condition=None",
			"--quiet",
		); err != nil {
			return err
		}
		role := role
		testEnv.cleanup.push(func() error { return removeProjectIAMBinding(role) })
	}
	return nil
}

func createKarpenterSAKey() error {
	keyPath := filepath.Join(testEnv.tempDir, "karpenter-sa-key.json")
	if err := runCommand(testEnv.repoRoot, cloudEnv(nil), "gcloud", "iam", "service-accounts", "keys", "create", keyPath,
		"--iam-account", testEnv.karpenterGSAEmail,
		"--project", testEnv.projectID,
		"--quiet",
	); err != nil {
		return fmt.Errorf("creating karpenter SA key: %w", err)
	}
	testEnv.karpenterKeyPath = keyPath
	return nil
}

func createCluster(ctx context.Context) error {
	parent := fmt.Sprintf("projects/%s/locations/%s", testEnv.projectID, testEnv.zone)
	networkLink := fmt.Sprintf("projects/%s/global/networks/%s", testEnv.projectID, testEnv.networkName)
	subnetworkLink := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", testEnv.projectID, testEnv.region, testEnv.subnetworkName)
	cluster := &container.Cluster{
		Name:              testEnv.clusterName,
		InitialNodeCount:  1,
		Network:           networkLink,
		Subnetwork:        subnetworkLink,
		Locations:         []string{testEnv.zone},
		LoggingService:    "logging.googleapis.com/kubernetes",
		MonitoringService: "monitoring.googleapis.com/kubernetes",
		NodeConfig: &container.NodeConfig{
			MachineType: defaultNodeMachineType,
			DiskSizeGb:  30,
			OauthScopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
		},
		IpAllocationPolicy: &container.IPAllocationPolicy{
			UseIpAliases:               true,
			ClusterSecondaryRangeName:  testEnv.podsRangeName,
			ServicesSecondaryRangeName: testEnv.servicesRangeName,
		},
		ReleaseChannel: &container.ReleaseChannel{Channel: "REGULAR"},
		WorkloadIdentityConfig: &container.WorkloadIdentityConfig{
			WorkloadPool: fmt.Sprintf("%s.svc.id.goog", testEnv.projectID),
		},
	}
	op, err := testEnv.containerService.Projects.Locations.Clusters.Create(parent, &container.CreateClusterRequest{Cluster: cluster}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("creating cluster %s: %w", testEnv.clusterName, err)
	}
	testEnv.cleanup.push(func() error { return deleteCluster(context.Background()) })
	if err := waitForContainerOperation(ctx, op.Name); err != nil {
		return fmt.Errorf("waiting for cluster create: %w", err)
	}
	return waitForClusterStatus(ctx, "RUNNING")
}

func removeDefaultNodePools(ctx context.Context) error {
	cluster, err := getCluster(ctx)
	if err != nil {
		return err
	}
	for _, nodePool := range cluster.NodePools {
		if nodePool == nil || nodePool.Name == "" {
			continue
		}
		name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", testEnv.projectID, testEnv.zone, testEnv.clusterName, nodePool.Name)
		op, err := testEnv.containerService.Projects.Locations.Clusters.NodePools.Delete(name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("deleting default node pool %s: %w", nodePool.Name, err)
		}
		if err := waitForContainerOperation(ctx, op.Name); err != nil {
			return fmt.Errorf("waiting for node pool delete %s: %w", nodePool.Name, err)
		}
	}
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, clusterDeleteTimeout, true, func(ctx context.Context) (bool, error) {
		cluster, err := getCluster(ctx)
		if err != nil {
			return false, err
		}
		return len(cluster.NodePools) == 0, nil
	})
}

func getClusterCredentials() error {
	return runCommand(testEnv.repoRoot, cloudEnv(nil), "gcloud", "container", "clusters", "get-credentials", testEnv.clusterName,
		"--zone", testEnv.zone,
		"--project", testEnv.projectID,
		"--quiet",
	)
}

func createNamespacesAndKSA(ctx context.Context) error {
	for _, ns := range []string{karpenterNamespace, testEnv.testNamespace} {
		_, err := testEnv.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating namespace %s: %w", ns, err)
		}
	}
	testEnv.cleanup.push(func() error { return deleteNamespace(context.Background(), testEnv.testNamespace) })
	testEnv.cleanup.push(func() error { return deleteNamespace(context.Background(), karpenterNamespace) })

	_, err := testEnv.clientset.CoreV1().ServiceAccounts(karpenterNamespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: karpenterServiceAccount},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating service account %s/%s: %w", karpenterNamespace, karpenterServiceAccount, err)
	}
	testEnv.cleanup.push(func() error { return deleteServiceAccountKSA(context.Background()) })
	return nil
}

func addWorkloadIdentityBinding() error {
	if err := runCommand(testEnv.repoRoot, cloudEnv(nil), "gcloud", "iam", "service-accounts", "add-iam-policy-binding", testEnv.karpenterGSAEmail,
		"--role", "roles/iam.workloadIdentityUser",
		"--member", testEnv.karpenterKSAMember,
		"--project", testEnv.projectID,
		"--quiet",
	); err != nil {
		return err
	}
	testEnv.cleanup.push(func() error { return removeWorkloadIdentityBinding() })
	return nil
}

func startController() error {
	cmd := exec.Command("go", "run", "./cmd/controller/main.go")
	cmd.Dir = testEnv.repoRoot
	cmd.Env = append(os.Environ(),
		"SYSTEM_NAMESPACE="+karpenterNamespace,
		"KUBERNETES_MIN_VERSION=v1.26.0",
		"DISABLE_LEADER_ELECTION=true",
		"CLUSTER_NAME="+testEnv.clusterName,
		"PROJECT_ID="+testEnv.projectID,
		"CLUSTER_LOCATION="+testEnv.zone,
		"INTERRUPTION_QUEUE="+testEnv.clusterName,
		"FEATURE_GATES=SpotToSpotConsolidation=true",
		"GOOGLE_APPLICATION_CREDENTIALS="+testEnv.karpenterKeyPath,
		"KUBECONFIG="+testEnv.kubeconfigPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting controller subprocess: %w", err)
	}
	testEnv.controllerCmd = cmd
	testEnv.controllerDone = make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = fmt.Errorf("controller exited unexpectedly")
		}
		testEnv.controllerDone <- err
	}()
	testEnv.cleanup.push(func() error { return stopController() })
	return nil
}

func waitForControllerReady(ctx context.Context) error {
	readyCtx, cancel := context.WithTimeout(ctx, controllerStartupTimeout)
	defer cancel()
	return wait.PollUntilContextTimeout(readyCtx, 5*time.Second, controllerStartupTimeout, true, func(ctx context.Context) (bool, error) {
		if err := controllerProcessError(); err != nil {
			return false, err
		}
		for _, name := range []string{"karpenter-default", "karpenter-ubuntu"} {
			nodePoolName := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s", testEnv.projectID, testEnv.zone, testEnv.clusterName, name)
			nodePool, err := testEnv.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolName).Context(ctx).Do()
			if isNotFound(err) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("getting controller template node pool %s: %w", name, err)
			}
			if nodePool.Status != "RUNNING" {
				return false, nil
			}
		}
		return true, nil
	})
}

func stopController() error {
	if testEnv.controllerCmd == nil {
		return nil
	}
	if err := controllerProcessError(); err != nil {
		return nil
	}
	if testEnv.controllerCmd.Process == nil {
		return nil
	}
	_ = testEnv.controllerCmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-testEnv.controllerDone:
		if err != nil && !strings.Contains(err.Error(), "signal: terminated") && !strings.Contains(err.Error(), "killed") {
			return err
		}
		return nil
	case <-time.After(15 * time.Second):
	}
	if err := testEnv.controllerCmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
		return fmt.Errorf("killing controller: %w", err)
	}
	select {
	case <-testEnv.controllerDone:
	case <-time.After(5 * time.Second):
	}
	return nil
}

func controllerProcessError() error {
	if testEnv.controllerExitErr != nil {
		return testEnv.controllerExitErr
	}
	select {
	case err := <-testEnv.controllerDone:
		testEnv.controllerExitErr = err
		return err
	default:
		return nil
	}
}

func deleteSuiteResourcesFromCluster(ctx context.Context) error {
	if testEnv.dynamicClient == nil || testEnv.clientset == nil {
		return nil
	}
	if err := deleteProvisioningResources(ctx); err != nil {
		return err
	}
	return nil
}

func deleteProvisioningResources(ctx context.Context) error {
	for _, deleteFn := range []func(context.Context) error{deleteNodeClaimsByPrefix, deleteNodePoolsByPrefix, deleteNodeClassesByPrefix} {
		if err := deleteFn(ctx); err != nil {
			return err
		}
	}
	return nil
}

func deleteNodeClaimsByPrefix(ctx context.Context) error {
	return deleteDynamicResourcesByPrefix(ctx, nodeClaimGVR)
}

func deleteNodePoolsByPrefix(ctx context.Context) error {
	return deleteDynamicResourcesByPrefix(ctx, nodePoolGVR)
}

func deleteNodeClassesByPrefix(ctx context.Context) error {
	return deleteDynamicResourcesByPrefix(ctx, gceNodeClassGVR)
}

func deleteDynamicResourcesByPrefix(ctx context.Context, gvr schema.GroupVersionResource) error {
	resources, err := testEnv.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing %s: %w", gvr.Resource, err)
	}
	for _, item := range resources.Items {
		if !strings.HasPrefix(item.GetName(), testEnv.prefix) {
			continue
		}
		if err := testEnv.dynamicClient.Resource(gvr).Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %s/%s: %w", gvr.Resource, item.GetName(), err)
		}
	}
	return nil
}

func deleteNamespace(ctx context.Context, name string) error {
	if testEnv.clientset == nil {
		return nil
	}
	err := testEnv.clientset.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func deleteServiceAccountKSA(ctx context.Context) error {
	if testEnv.clientset == nil {
		return nil
	}
	err := testEnv.clientset.CoreV1().ServiceAccounts(karpenterNamespace).Delete(ctx, karpenterServiceAccount, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func deleteCluster(ctx context.Context) error {
	if testEnv.clusterName == "" || testEnv.containerService == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, suiteCleanupTimeout)
	defer cancel()
	_ = deleteSuiteResourcesFromCluster(cleanupCtx)
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", testEnv.projectID, testEnv.zone, testEnv.clusterName)
	op, err := testEnv.containerService.Projects.Locations.Clusters.Delete(name).Context(ctx).Do()
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting cluster %s: %w", testEnv.clusterName, err)
	}
	if err := waitForContainerOperation(ctx, op.Name); err != nil {
		return fmt.Errorf("waiting for cluster delete: %w", err)
	}
	return waitForClusterDeleted(ctx)
}

func deleteSubnetwork(ctx context.Context) error {
	if testEnv.subnetworkName == "" || testEnv.computeService == nil {
		return nil
	}
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, computeDeleteTimeout, true, func(ctx context.Context) (bool, error) {
		op, err := testEnv.computeService.Subnetworks.Delete(testEnv.projectID, testEnv.region, testEnv.subnetworkName).Context(ctx).Do()
		if isNotFound(err) {
			return true, nil
		}
		if err != nil {
			if isResourceInUse(err) {
				return false, nil
			}
			return false, err
		}
		if err := waitForRegionalComputeOperation(ctx, testEnv.region, op.Name); err != nil {
			if isResourceInUse(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func deleteNetwork(ctx context.Context) error {
	if testEnv.networkName == "" || testEnv.computeService == nil {
		return nil
	}
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, computeDeleteTimeout, true, func(ctx context.Context) (bool, error) {
		op, err := testEnv.computeService.Networks.Delete(testEnv.projectID, testEnv.networkName).Context(ctx).Do()
		if isNotFound(err) {
			return true, nil
		}
		if err != nil {
			if isResourceInUse(err) {
				return false, nil
			}
			return false, err
		}
		if err := waitForGlobalComputeOperation(ctx, op.Name); err != nil {
			if isResourceInUse(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func removeWorkloadIdentityBinding() error {
	return runCommandIgnoreMissing(testEnv.repoRoot, cloudEnv(nil), "gcloud", "iam", "service-accounts", "remove-iam-policy-binding", testEnv.karpenterGSAEmail,
		"--role", "roles/iam.workloadIdentityUser",
		"--member", testEnv.karpenterKSAMember,
		"--project", testEnv.projectID,
		"--quiet",
	)
}

func removeProjectIAMBinding(role string) error {
	return runCommandIgnoreMissing(testEnv.repoRoot, cloudEnv(nil), "gcloud", "projects", "remove-iam-policy-binding", testEnv.projectID,
		"--member", "serviceAccount:"+testEnv.karpenterGSAEmail,
		"--role", role,
		"--condition=None",
		"--quiet",
	)
}

func deleteServiceAccount() error {
	return runCommandIgnoreMissing(testEnv.repoRoot, cloudEnv(nil), "gcloud", "iam", "service-accounts", "delete", testEnv.karpenterGSAEmail,
		"--project", testEnv.projectID,
		"--quiet",
	)
}

func verifyProjectClean(ctx context.Context) error {
	if testEnv.projectID == "" || testEnv.computeService == nil || testEnv.containerService == nil {
		return nil
	}
	if leftovers, err := findLeftoverClusters(ctx); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover clusters: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverNetworks(ctx); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover networks: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverSubnetworks(ctx); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover subnetworks: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverFirewalls(ctx); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover firewalls: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverRoutes(ctx); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover routes: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverInstances(ctx); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover instances: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverServiceAccounts(); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover service accounts: %s", strings.Join(leftovers, ", "))
	}
	if leftovers, err := findLeftoverIAMMembers(); err != nil {
		return err
	} else if len(leftovers) > 0 {
		return fmt.Errorf("leftover IAM members: %s", strings.Join(leftovers, ", "))
	}
	return nil
}

func findLeftoverClusters(ctx context.Context) ([]string, error) {
	parent := fmt.Sprintf("projects/%s/locations/-", testEnv.projectID)
	resp, err := testEnv.containerService.Projects.Locations.Clusters.List(parent).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing clusters: %w", err)
	}
	var leftovers []string
	for _, cluster := range resp.Clusters {
		if cluster != nil && strings.HasPrefix(cluster.Name, testEnv.prefix) {
			leftovers = append(leftovers, cluster.Name)
		}
	}
	return leftovers, nil
}

func findLeftoverNetworks(ctx context.Context) ([]string, error) {
	resp, err := testEnv.computeService.Networks.List(testEnv.projectID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	return namesWithPrefix(resp.Items, testEnv.prefix, func(item *compute.Network) string { return item.Name }), nil
}

func findLeftoverSubnetworks(ctx context.Context) ([]string, error) {
	resp, err := testEnv.computeService.Subnetworks.AggregatedList(testEnv.projectID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing subnetworks: %w", err)
	}
	var leftovers []string
	for _, scoped := range resp.Items {
		leftovers = append(leftovers, namesWithPrefix(scoped.Subnetworks, testEnv.prefix, func(item *compute.Subnetwork) string { return item.Name })...)
	}
	return leftovers, nil
}

func findLeftoverFirewalls(ctx context.Context) ([]string, error) {
	resp, err := testEnv.computeService.Firewalls.List(testEnv.projectID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing firewalls: %w", err)
	}
	return namesWithPrefix(resp.Items, testEnv.prefix, func(item *compute.Firewall) string { return item.Name }), nil
}

func findLeftoverRoutes(ctx context.Context) ([]string, error) {
	resp, err := testEnv.computeService.Routes.List(testEnv.projectID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing routes: %w", err)
	}
	return namesWithPrefix(resp.Items, testEnv.prefix, func(item *compute.Route) string { return item.Name }), nil
}

func findLeftoverInstances(ctx context.Context) ([]string, error) {
	resp, err := testEnv.computeService.Instances.AggregatedList(testEnv.projectID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	var leftovers []string
	for _, scoped := range resp.Items {
		leftovers = append(leftovers, namesWithPrefix(scoped.Instances, testEnv.prefix, func(item *compute.Instance) string { return item.Name })...)
	}
	return leftovers, nil
}

func findLeftoverServiceAccounts() ([]string, error) {
	output, err := commandOutput(testEnv.repoRoot, cloudEnv(nil), "gcloud", "iam", "service-accounts", "list",
		"--project", testEnv.projectID,
		"--format=value(email)",
	)
	if err != nil {
		return nil, err
	}
	var leftovers []string
	for _, line := range splitLines(output) {
		if strings.HasPrefix(line, testEnv.prefix) || strings.Contains(line, "/"+testEnv.prefix) {
			leftovers = append(leftovers, line)
		}
	}
	return leftovers, nil
}

func findLeftoverIAMMembers() ([]string, error) {
	output, err := commandOutput(testEnv.repoRoot, cloudEnv(nil), "gcloud", "projects", "get-iam-policy", testEnv.projectID,
		"--flatten=bindings[].members",
		"--format=value(bindings.members)",
	)
	if err != nil {
		return nil, err
	}
	var leftovers []string
	for _, line := range splitLines(output) {
		if strings.Contains(line, testEnv.prefix) {
			leftovers = append(leftovers, line)
		}
	}
	return leftovers, nil
}

func waitForClusterStatus(ctx context.Context, status string) error {
	ctx, cancel := context.WithTimeout(ctx, clusterCreateTimeout)
	defer cancel()
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, clusterCreateTimeout, true, func(ctx context.Context) (bool, error) {
		cluster, err := getCluster(ctx)
		if err != nil {
			return false, err
		}
		if cluster.Status == "ERROR" {
			return false, fmt.Errorf("cluster entered ERROR state: %s", cluster.StatusMessage)
		}
		return cluster.Status == status, nil
	})
}

func waitForClusterDeleted(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, clusterDeleteTimeout)
	defer cancel()
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, clusterDeleteTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := getCluster(ctx)
		if isNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func getCluster(ctx context.Context) (*container.Cluster, error) {
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", testEnv.projectID, testEnv.zone, testEnv.clusterName)
	cluster, err := testEnv.containerService.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("getting cluster %s: %w", testEnv.clusterName, err)
	}
	return cluster, nil
}

func waitForContainerOperation(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, clusterCreateTimeout)
	defer cancel()
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, clusterCreateTimeout, true, func(ctx context.Context) (bool, error) {
		op, err := testEnv.containerService.Projects.Locations.Operations.Get(name).Context(ctx).Do()
		if err != nil {
			return false, fmt.Errorf("getting container operation %s: %w", name, err)
		}
		if op.Status != "DONE" {
			return false, nil
		}
		if op.Error != nil {
			return false, fmt.Errorf("container operation %s failed: %s", name, op.Error.Message)
		}
		return true, nil
	})
}

func waitForGlobalComputeOperation(ctx context.Context, name string) error {
	return waitForComputeOperation(ctx, func(ctx context.Context) (*compute.Operation, error) {
		return testEnv.computeService.GlobalOperations.Get(testEnv.projectID, name).Context(ctx).Do()
	})
}

func waitForRegionalComputeOperation(ctx context.Context, region, name string) error {
	return waitForComputeOperation(ctx, func(ctx context.Context) (*compute.Operation, error) {
		return testEnv.computeService.RegionOperations.Get(testEnv.projectID, region, name).Context(ctx).Do()
	})
}

func waitForComputeOperation(ctx context.Context, get func(context.Context) (*compute.Operation, error)) error {
	ctx, cancel := context.WithTimeout(ctx, computeDeleteTimeout)
	defer cancel()
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, computeDeleteTimeout, true, func(ctx context.Context) (bool, error) {
		op, err := get(ctx)
		if err != nil {
			return false, err
		}
		if op.Status != "DONE" {
			return false, nil
		}
		if op.Error != nil && len(op.Error.Errors) > 0 {
			var messages []string
			for _, item := range op.Error.Errors {
				if item == nil {
					continue
				}
				if item.Message != "" {
					messages = append(messages, item.Message)
				} else {
					messages = append(messages, item.Code)
				}
			}
			return false, errors.New(strings.Join(messages, "; "))
		}
		return true, nil
	})
}

func loadRESTConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if os.Getenv("KUBECONFIG") == "" {
		kubeconfigPath := strings.TrimSpace(testEnv.kubeconfigPath)
		if kubeconfigPath == "" {
			kubeconfigPath = filepath.Join(homedir.HomeDir(), ".kube", "config")
		}
		loadingRules.ExplicitPath = kubeconfigPath
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locating setup_test.go")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func runCommand(workdir string, env map[string]string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = mergeEnv(env)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func commandOutput(workdir string, env map[string]string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = mergeEnv(env)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("running %s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func runCommandIgnoreMissing(workdir string, env map[string]string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = mergeEnv(env)
	output, err := cmd.CombinedOutput()
	if err != nil {
		lower := strings.ToLower(string(output))
		if strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist") || strings.Contains(lower, "no policy binding") {
			return nil
		}
		return fmt.Errorf("running %s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cloudEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"GOOGLE_APPLICATION_CREDENTIALS":         testEnv.credentialsPath,
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE": testEnv.credentialsPath,
		"CLOUDSDK_CORE_PROJECT":                  testEnv.projectID,
		"KUBECONFIG":                             testEnv.kubeconfigPath,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func mergeEnv(overrides map[string]string) []string {
	base := map[string]string{}
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		base[key] = value
	}
	for key, value := range overrides {
		base[key] = value
	}
	result := make([]string, 0, len(base))
	for key, value := range base {
		result = append(result, key+"="+value)
	}
	return result
}

func isNotFound(err error) bool {
	var gErr *googleapi.Error
	return errors.As(err, &gErr) && gErr.Code == 404
}

func isResourceInUse(err error) bool {
	var gErr *googleapi.Error
	if !errors.As(err, &gErr) {
		return false
	}
	if gErr.Code != 400 && gErr.Code != 409 {
		return false
	}
	message := strings.ToLower(gErr.Message)
	return strings.Contains(message, "resourceinuse") || strings.Contains(message, "being used") || strings.Contains(message, "in use")
}

func namesWithPrefix[T any](items []T, prefix string, nameFn func(T) string) []string {
	var names []string
	for _, item := range items {
		name := nameFn(item)
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names
}

func splitLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func sanitizeName(value string, maxLen int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", "-", "/", "-", ".", "-", " ", "-").Replace(value)
	var b strings.Builder
	lastDash := false
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
			lastDash = false
		case ch == '-':
			if !lastDash {
				b.WriteRune(ch)
				lastDash = true
			}
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > maxLen {
		result = strings.Trim(result[:maxLen], "-")
	}
	return result
}

func gcpName(prefix, suffix string, maxLen int) string {
	name := sanitizeName(prefix+"-"+suffix, maxLen)
	if name == "" {
		return sanitizeName(suffix, maxLen)
	}
	return name
}
