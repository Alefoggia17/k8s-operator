# k8s-operator && Time Series Forecasting

A utility for provisioning ARM64 virtual machines using `libvirt`/`KVM` and deploying custom Kubernetes operators in an automated fashion.

## Description

This project combines virtual infrastructure setup through a `bash` script with automated deployment of Kubernetes operators (e.g., `scaler-operator`, `prometheus-operator`). It is designed to run ARM64-based virtual machines—ideal for edge computing or Kubernetes testing scenarios—using Ubuntu 20.04 cloud images and integrates them into a cluster environment.
Additionally, this project includes evaluating application time series (metrics such as CPU or memory consumption) for proactively assessing load spikes.

## Prerequisites for k8s-operator

- `virt-install`, `qemu-img`, `libvirt`, `virsh`
- `wget`, `bash`, `qemu-utils`
- `sudo` privileges
- Internet access to download cloud images
- A working Kubernetes v1.11.3+ cluster
- `go` v1.23.0+, `docker`, and `kubectl`

---

### 1. Virtual Machine Provisioning

The `create_vms.sh` script automates the creation of 3 Ubuntu 20.04 ARM64 VMs.

To run the script:

```sh
bash create_vms.sh

```
### 2. Follow instructions

Please, follow the istructions in `prometheus-operator/README.md` and `scaler-operator/README.md` and read the PDF file.


## Time Series Forecasting

In `TimeSeriesForecasting` directory, there are several **.py** and **.ipynb** files. These files are used to create the datasets for training the **CONV + LSTM** model. You can use the functions in the `functions.py` file to reproduce the various datasets. Once this is done, you need to use the `tsf.py` file to implement time series forecasting.

In the `.ipynb` files you can run everything directly, changing the relative paths.



