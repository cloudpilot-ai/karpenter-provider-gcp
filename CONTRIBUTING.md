# Development

This doc explains how to set up a development environment so you can get started contributing to this project

## Prerequisites

1. [`go`](https://golang.org/doc/install): For building the project
1. [`git`](https://help.github.com/articles/set-up-git/): For source control
1. [`gcloud`](https://cloud.google.com/sdk/docs/install): For Interacting with the Google Cloud Platform

This guides breaks down into two parts 
1. Setting up the GKE cluster - If you have already setup the GKE cluster, you can skip this part
2. Setting up the development environment - This part is required for all the contributors

## Setting up the GKE cluster

1. First Go ahead and create new project in the Google Cloud Platform and enable billing for the project if you don't have it already enabled. You can follow the below links as guide on how to create a new project and enable billing for the project
    - [Creating the project](https://cloud.google.com/resource-manager/docs/creating-managing-projects)
    - [Enable billing for given project](https://cloud.google.com/billing/docs/how-to/modify-project)


2. Once you have created the project enable the below apis 

    ```
    gcloud services enable compute.googleapis.com
    gcloud services enable container.googleapis.com
    ```

3. Next go ahead and create the service account in the project and assign the following role to the service account

    - Kubernetes Engine Admin
    - Monitoring Admin
    - Service Account User


4. Once you have assigned the roles to the service account, go ahead and create a new key for the service account by following this link as a guide [Creating and managing service account keys](https://cloud.google.com/iam/docs/creating-managing-service-account-keys)


    > Note : Make sure you have the service account key stored in a secure location and do not share it with anyone.

5. Now configure the gcloud sdk to use the service account key by running the below command

    ```
    gcloud auth activate-service-account --key-file /path/to/key.json
    ```

6. First configure the required environment variable using the below command

    ```bash
    export PROJECT_ID=<project-id>
    export CLUSTER_NAME=<cluster-name>
    export REGION=<region>
    ```

7. Next create a GKE cluster using the below command
    ```
    gcloud container clusters create $CLUSTER_NAME --location=$REGION --num-nodes 1 --machine-type=n1-standard-1 --disk-size=10 --project $PROJECT_ID 
    ```


8. Once the cluster is created, go ahead and get the credentials for the cluster by running the below command

    ```
    gcloud container clusters get-credentials $CLUSTER_NAME --location=$REGION --project $PROJECT_ID
    ```

9. Now export the generated kubeconfig file to the `KUBECONFIG` environment variable by running the below command


    ```
    export KUBECONFIG=~/.kube/config
    ```


10. Next export the service account key to the `GOOGLE_APPLICATION_CREDENTIALS` environment variable by running the below command


    ```
    export GOOGLE_APPLICATION_CREDENTIALS="/path/to/key.json"
    ```

## Setting up the development environment

1. If you already setup the PROJECT_ID ,REGION and CLUSTER_NAME environment variables in the while setting up the GKE cluster, you can skip this step. If not, go ahead and export the below environment variables

    ```bash
    export PROJECT_ID=<project-id>
    export REGION=<region>
    export CLUSTER_NAME=<cluster-name>
    ```

2. Once you have exported the environment variables, go ahead and install the CRDs for the karpenter by running the below command

    ```
    kubectl apply -f charts/karpenter/crds/
    ```

3. Once you have installed the CRDs, go ahead and install the karpenter by running the below command
    ```
    make run 
    ```