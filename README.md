# k8s-operator

A utility for provisioning ARM64 virtual machines using `libvirt`/`KVM` and deploying custom Kubernetes operators in an automated fashion.

## Description

This project combines virtual infrastructure setup through a `bash` script with automated deployment of Kubernetes operators (e.g., `scaler-operator`, `prometheus-operator`). It is designed to run ARM64-based virtual machines—ideal for edge computing or Kubernetes testing scenarios—using Ubuntu 20.04 cloud images and integrates them into a cluster environment.

## Getting Started

### Prerequisites

- `virt-install`, `qemu-img`, `libvirt`, `virsh`
- `wget`, `bash`, `qemu-utils`
- `sudo` privileges
- Internet access to download cloud images
- A working Kubernetes v1.11.3+ cluster
- `go` v1.23.0+, `docker`, and `kubectl`

---

## 1. Virtual Machine Provisioning

The `create_vms.sh` script automates the creation of 3 Ubuntu 20.04 ARM64 VMs.

To run the script:

```sh
bash create_vms.sh

```
## 2. Follow instructions

Please, follow the istructions in `prometheus-operator/README.md` and `scaler-operator/README.md` and read the PDF file.
