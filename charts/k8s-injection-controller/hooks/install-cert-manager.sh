#!/usr/bin/env sh

if kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
    echo "Detected an existing certificate manager. Doing nothing"
else
    echo "Installing a certificate manager"

    helm repo add jetstack https://charts.jetstack.io

    helm repo update

    helm install cert-manager jetstack/cert-manager \
      --namespace cert-manager \
      --create-namespace \
      --set crds.enabled=true

    echo "Waiting for cert-manager-webhook to be ready"
    kubectl -n cert-manager rollout status deployment/cert-manager-webhook

    echo "Successful installation of Certificate Manager."
fi
