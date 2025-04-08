## Development

This doc explains how to set up a development environment so you can get started contributing to this project

## Install Requirements

1. [`go`](https://golang.org/doc/install): For building the project
1. [`git`](https://help.github.com/articles/set-up-git/): For source control
1. [`gcloud`](https://cloud.google.com/sdk/docs/install): For Interacting with the Google Cloud Platform


## Setup

1. First Go ahead and create new project in the Google Cloud Platform and enable billing for the project

2. Once you have created the project enable the below apis 

```
gcloud services enable compute.googleapis.com
gcloud services enable container.googleapis.com
```

3. Next go ahead and create the service account in the project and assign `OWNER` role and create the service account key by following this [link](https://cloud.google.com/iam/docs/creating-managing-service-account-keys)

> Note : For now we are assigning the `OWNER` role to the service account. We are working on the more fine grained permissions for the service account. So please assign the `OWNER` role for now and we will update the documentation once we have the fine grained permissions ready.


4. Once you have created the service account key, go ahead and download the json file and stored it somewhere secure in your local machine

> Note : Make sure you have the service account key stored in a secure location and do not share it with anyone.

5. Now configure the gcloud sdk to use the service account key by running the below command

```
gcloud auth activate-service-account --key-file /path/to/key.json
```

6. Next go ahead and create a new cluster in the project by running the below command



```bash
export PROJECT_ID=<project-id>
export CLUSTER_NAME=<cluster-name>
export REGION=<region>
```


```
gcloud container clusters create $CLUSTER_NAME --location=$REGION --num-nodes 1 --machine-type=n1-standard-1 --disk-size=10 --project $PROJECT_ID 
```


7. Once the cluster is created, go ahead and get the credentials for the cluster by running the below command

```
gcloud container clusters get-credentials $CLUSTER_NAME --location=$REGION --project $PROJECT_ID
```

8. Now export the `GOOGLE_APPLICATION_CREDENTIALS` environment variable to point to the service account key file by running the below command

```
export GOOGLE_APPLICATION_CREDENTIALS="/path/to/key.json"
```

9. Next setup the below environment variables

```
export KUBE_CONFIG=~/.kube/config
```

10. Optional : If you already setup the PROJECT_ID ,REGION and CLUSTER_NAME environment variables in the previous steps, you can skip this step. Otherwise, you can set the below environment variables

```bash
export PROJECT_ID=<project-id>
export REGION=<region>
export CLUSTER_NAME=<cluster-name>
```


Now you are all set to start developing the project. You can now run the project by running the below command

```
go run cmd/controller/main.go
```
