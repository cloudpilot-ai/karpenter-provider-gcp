![GitHub stars](https://img.shields.io/github/stars/cloudpilot-ai/karpenter-provider-gcp)
![GitHub forks](https://img.shields.io/github/forks/cloudpilot-ai/karpenter-provider-gcp)
[![GitHub License](https://img.shields.io/badge/License-Apache%202.0-ff69b4.svg)](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/LICENSE)
[![contributions welcome](https://img.shields.io/badge/contributions-welcome-brightgreen.svg?style=flat)](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues)

<p align="center">
    <img src="docs/images/banner.png" height="200" />
</p>

> [!NOTE]  
> It’s not available for use temporarily. We are diligently working on it, and it will be available shortly.

Karpenter is an open-source node provisioning project built for Kubernetes.
Karpenter improves the efficiency and cost of running workloads on Kubernetes clusters by:

* **Watching** for pods that the Kubernetes scheduler has marked as unschedulable
* **Evaluating** scheduling constraints (resource requests, nodeselectors, affinities, tolerations, and topology spread constraints) requested by the pods
* **Provisioning** nodes that meet the requirements of the pods
* **Removing** the nodes when the nodes are no longer needed

Come discuss Karpenter in the [#karpenter](https://kubernetes.slack.com/archives/C02SFFZSA2K) channel, in the [Kubernetes slack](https://slack.k8s.io/) or join the [Karpenter working group](https://karpenter.sh/docs/contributing/working-group/) bi-weekly calls. If you want to contribute to the Karpenter project, please refer to the Karpenter docs.

