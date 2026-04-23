# Quick start

This guide creates a minimal GCENodeClass and NodePool, deploys a test workload, and verifies that Karpenter provisions a node.

## Prerequisites

Karpenter must be installed. See [Installation](installation.md).

## Step 1 — Create a GCENodeClass

A `GCENodeClass` captures GCP-specific node configuration: image, disk, service account, and network settings.

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: default
spec:
  imageSelectorTerms:
    - alias: ContainerOptimizedOS@latest
  disks:
    - category: pd-balanced
      sizeGiB: 60
      boot: true
```

> **Node service account:** If `spec.serviceAccount` is not set, Karpenter falls back to the `--node-pool-service-account` operator flag, then to the project's Compute Engine default SA. For production clusters, GKE recommends a dedicated SA with only [`roles/container.nodeServiceAccount`](https://cloud.google.com/kubernetes-engine/docs/how-to/hardening-your-cluster#use_least_privilege_sa). Set `spec.serviceAccount` in the NodeClass or configure `NODE_POOL_SERVICE_ACCOUNT` on the Karpenter deployment.

Apply it:

```sh
kubectl apply -f gcenodeclass.yaml
```

Verify it becomes ready:

```sh
kubectl get gcenodeclass default
```

```
NAME      READY   AGE
default   True    10s
```

## Step 2 — Create a NodePool

A `NodePool` defines scheduling requirements and links to a GCENodeClass.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.gcp
        kind: GCENodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
        - key: karpenter.k8s.gcp/instance-family
          operator: In
          values: ["n4", "n2", "e2"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 0s
```

Apply it:

```sh
kubectl apply -f nodepool.yaml
```

## Step 3 — Deploy a test workload

Deploy a lightweight pod that requests enough resources to trigger provisioning:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inflate
spec:
  replicas: 5
  selector:
    matchLabels:
      app: inflate
  template:
    metadata:
      labels:
        app: inflate
    spec:
      containers:
        - name: inflate
          image: public.ecr.aws/eks-distro/kubernetes/pause:3.2
          resources:
            requests:
              cpu: 500m
              memory: 512Mi
```

Apply it:

```sh
kubectl apply -f inflate.yaml
```

## Step 4 — Verify provisioning

Watch for a new NodeClaim and Node to appear:

```sh
kubectl get nodeclaim
```

```
NAME               TYPE          CAPACITY   ZONE           NODE                          READY   AGE
default-abc12      e2-standard-4 spot       us-central1-a  karpenter-default-abc12       True    30s
```

```sh
kubectl get nodes
```

The Karpenter-provisioned node has a name beginning with `karpenter-`.

Check pod scheduling:

```sh
kubectl get pods -o wide
```

All `inflate` pods should be `Running` on the new node within about a minute.

## Cleaning up

Delete the test deployment to trigger node consolidation and removal:

```sh
kubectl delete deployment inflate
```

Karpenter will drain and delete the node once it is empty.

## Next steps

- [GCENodeClass reference](../reference/gcenodeclass.md) — all available configuration fields
- [NodePool examples](../examples/default.md) — spot, arm64, GPU, private nodes, and more
