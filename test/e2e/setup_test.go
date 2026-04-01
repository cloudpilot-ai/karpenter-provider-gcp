package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	defaultRegion      = "us-central1"
	karpenterNamespace = "karpenter-system"
	testNamespace      = "karpenter-e2e"
)

type suiteState struct {
	projectID        string
	credentialsPath  string
	prefix           string
	region           string
	repoRoot         string
	terraformDir     string
	chartPath        string
	crdPath          string
	kubeconfigPath   string
	clusterName      string
	karpenterSAEmail string
	zones            []string
	terraformApplied bool
	clientset        kubernetes.Interface
	dynamicClient    dynamic.Interface
}

type gkeClusterDescription struct {
	Locations []string `json:"locations"`
}

var testEnv suiteState

func TestMain(m *testing.M) {
	exitCode := 1

	if err := setupSuite(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
	} else {
		exitCode = m.Run()
	}

	if err := cleanupSuite(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e cleanup failed: %v\n", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}

	os.Exit(exitCode)
}

func setupSuite() error {
	credentialsPath := os.Getenv("E2E_GCP_CREDENTIALS")
	if credentialsPath == "" {
		return fmt.Errorf("E2E_GCP_CREDENTIALS must be set")
	}
	credentialsPath, err := filepath.Abs(credentialsPath)
	if err != nil {
		return fmt.Errorf("resolving credentials path: %w", err)
	}
	if _, err := os.Stat(credentialsPath); err != nil {
		return fmt.Errorf("stat credentials path: %w", err)
	}

	repoRoot, err := repoRoot()
	if err != nil {
		return err
	}

	projectID := os.Getenv("E2E_PROJECT_ID")
	if projectID == "" {
		return fmt.Errorf("E2E_PROJECT_ID must be set")
	}
	prefix := os.Getenv("E2E_PREFIX")
	if prefix == "" {
		return fmt.Errorf("E2E_PREFIX must be set")
	}

	testEnv = suiteState{
		projectID:       projectID,
		credentialsPath: credentialsPath,
		prefix:          prefix,
		region:          defaultRegion,
		repoRoot:        repoRoot,
		terraformDir:    filepath.Join(repoRoot, "test", "e2e", "terraform"),
		chartPath:       filepath.Join(repoRoot, "charts", "karpenter"),
		crdPath:         filepath.Join(repoRoot, "charts", "karpenter", "crds"),
	}

	if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentialsPath); err != nil {
		return fmt.Errorf("setting GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}

	if err := runCommand(repoRoot, "gcloud", "auth", "activate-service-account", "--key-file", credentialsPath); err != nil {
		return err
	}
	if err := runCommand(testEnv.terraformDir, "terraform", "init", "-input=false"); err != nil {
		return err
	}
	if err := runCommand(testEnv.terraformDir, "terraform", append([]string{"apply", "-input=false", "-auto-approve"}, terraformVarArgs()...)...); err != nil {
		return err
	}
	testEnv.terraformApplied = true

	clusterName, err := terraformOutput("cluster_name")
	if err != nil {
		return err
	}
	karpenterSAEmail, err := terraformOutput("karpenter_sa_email")
	if err != nil {
		return err
	}
	kubeconfigPath, err := terraformOutput("kubeconfig_path")
	if err != nil {
		return err
	}

	testEnv.clusterName = clusterName
	testEnv.karpenterSAEmail = karpenterSAEmail
	testEnv.kubeconfigPath = kubeconfigPath

	if err := runCommand(repoRoot, "gcloud", "container", "clusters", "get-credentials", clusterName, "--region", testEnv.region, "--project", testEnv.projectID); err != nil {
		return err
	}
	if err := runCommand(repoRoot, "kubectl", "apply", "-f", testEnv.crdPath); err != nil {
		return err
	}
	if err := runCommand(repoRoot, "helm", "upgrade", "--install", "karpenter", testEnv.chartPath,
		"--namespace", karpenterNamespace,
		"--create-namespace",
		"--wait",
		"--timeout", "10m",
		"--set", "controller.replicaCount=1",
		"--set-string", fmt.Sprintf("controller.settings.projectID=%s", testEnv.projectID),
		"--set-string", fmt.Sprintf("controller.settings.clusterLocation=%s", testEnv.region),
		"--set-string", fmt.Sprintf("controller.settings.clusterName=%s", testEnv.clusterName),
		"--set", "credentials.enabled=false",
		"--set-string", fmt.Sprintf(`serviceAccount.annotations.iam\\.gke\\.io/gcp-service-account=%s`, testEnv.karpenterSAEmail),
	); err != nil {
		return err
	}

	testEnv.zones, err = discoverZones()
	if err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, err = testEnv.clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating test namespace: %w", err)
	}
	return nil
}

func cleanupSuite() error {
	if testEnv.clientset != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = testEnv.clientset.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
		cancel()
	}
	if testEnv.clusterName != "" {
		_ = runCommand(testEnv.repoRoot, "helm", "uninstall", "karpenter", "--namespace", karpenterNamespace)
	}
	if !testEnv.terraformApplied {
		return nil
	}
	return runCommand(testEnv.terraformDir, "terraform", append([]string{"destroy", "-input=false", "-auto-approve"}, terraformVarArgs()...)...)
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locating setup_test.go")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func terraformVarArgs() []string {
	return []string{
		"-var", fmt.Sprintf("project_id=%s", testEnv.projectID),
		"-var", fmt.Sprintf("credentials_path=%s", testEnv.credentialsPath),
		"-var", fmt.Sprintf("prefix=%s", testEnv.prefix),
		"-var", fmt.Sprintf("region=%s", testEnv.region),
	}
}

func terraformOutput(name string) (string, error) {
	output, err := commandOutput(testEnv.terraformDir, "terraform", "output", "-raw", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func discoverZones() ([]string, error) {
	output, err := commandOutput(testEnv.repoRoot, "gcloud", "container", "clusters", "describe", testEnv.clusterName,
		"--region", testEnv.region,
		"--project", testEnv.projectID,
		"--format=json",
	)
	if err != nil {
		return nil, err
	}

	description := &gkeClusterDescription{}
	if err := json.Unmarshal([]byte(output), description); err != nil {
		return nil, fmt.Errorf("parsing cluster description: %w", err)
	}
	if len(description.Locations) == 0 {
		return []string{
			fmt.Sprintf("%s-a", testEnv.region),
			fmt.Sprintf("%s-b", testEnv.region),
			fmt.Sprintf("%s-c", testEnv.region),
		}, nil
	}
	return description.Locations, nil
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

func runCommand(workdir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func commandOutput(workdir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("creating stdout pipe for %s: %w", name, err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting %s %s: %w", name, strings.Join(args, " "), err)
	}
	data, readErr := io.ReadAll(stdout)
	waitErr := cmd.Wait()
	if readErr != nil {
		return "", fmt.Errorf("reading %s stdout: %w", name, readErr)
	}
	if waitErr != nil {
		return "", fmt.Errorf("running %s %s: %w", name, strings.Join(args, " "), waitErr)
	}
	return string(data), nil
}
