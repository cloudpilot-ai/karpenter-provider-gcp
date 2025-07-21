# karpenter

A Helm chart for Karpenter, an open-source node provisioning project built for Kubernetes.

## Documentation

## Prepare the GCP Credentials

Create a Kubernetes Secret to store your GCP service account credentials:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: karpenter-gcp-credentials
  namespace: karpenter-system
type: Opaque
stringData:
  key.json: |
    {
      "type": "service_account",
      "project_id": "x",
      "private_key_id": "x",
      "private_key": "x",
      "client_email": "x",
      "client_id": "x",
      "auth_uri": "https://accounts.google.com/o/oauth2/auth",
      "token_uri": "https://oauth2.googleapis.com/token",
      "auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
      "client_x509_cert_url": "x",
      "universe_domain": "googleapis.com"
    }
```
Save the above as `karpenter-gcp-credentials.yaml`, then apply it to your cluster:
```sh
kubectl apply -f karpenter-gcp-credentials.ayml
```

Referring to [contribution](../CONTRIBUTING.md) to generate the GCP credentials.

## Installing the Chart

Run the following command the set the required environment variables before installing the chart:

```bash
export PROJECT_ID=<your-google-project-id>
export CLUSTER_NAME=<gke-cluster-name>
export REGION=<gke-region-name>
```

Then run the following command to install the chart:

```bash
helm upgrade karpenter charts/karpenter --install \
  --namespace karpenter-system --create-namespace \
  --set "controller.settings.projectID"="${PROJECT_ID}" \
  --set "controller.settings.region"="${REGION}" \
  --set "controller.settings.clusterName"="${CLUSTER_NAME}" \
  --wait
```

## Uninstalling the Chart

```bash
helm uninstall karpenter --namespace karpenter-system
kubectl delete ns karpenter-system
```
