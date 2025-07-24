#!/bin/bash

# Impostazioni di base
NUM_VM=3
BASE_IMAGE="/var/lib/libvirt/images/ubuntu-20.04-arm64.qcow2"
RAM=2048
DISK_SIZE=20G
IMAGE_URL="https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-arm64.img"

# Controlla se l'immagine di base esiste, altrimenti la scarica
if [ ! -f "$BASE_IMAGE" ]; then
    echo "Scaricando l'immagine di Ubuntu 20.04 ARM64..."
    sudo wget -O "$BASE_IMAGE" "$IMAGE_URL"
    sudo chown libvirt-qemu:kvm "$BASE_IMAGE"
    sudo chmod 644 "$BASE_IMAGE"
    echo "Download completato!"
fi

# Creazione delle VM
for i in $(seq 1 $NUM_VM); do
    VM_NAME="vm0${i}"
    VM_IP="192.168.85.19${i}"
    PORT=$((3000 + i))

    echo "Creazione della VM $VM_NAME con IP $VM_IP e porta SSH $PORT"

    # Creazione immagine disco
    sudo qemu-img create -f qcow2 -b "$BASE_IMAGE" "/var/lib/libvirt/images/${VM_NAME}.qcow2" -F qcow2 $DISK_SIZE

    # Creazione VM con QEMU/KVM
    sudo virt-install \
        --name "$VM_NAME" \
        --vcpus $(if [ $i -eq 2 ] || [ $i -eq 3 ]; then echo 1; else echo 2; fi) \
        --memory $RAM \
        --disk path="/var/lib/libvirt/images/${VM_NAME}.qcow2",format=qcow2 \
        --os-type linux \
        --os-variant ubuntu20.04 \
        --network network=default,model=virtio \
        --graphics none \
        --console pty,target_type=serial \
        --import \
        --arch aarch64 \
        --machine virt \
        --noautoconsole

    # Configurazione della rete
    sudo virsh net-update default add ip-dhcp-host "<host mac='$(sudo virsh domiflist $VM_NAME | grep -oE '([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}')' name='$VM_NAME' ip='$VM_IP'/>" --live --config

    echo "VM $VM_NAME creata con successo!"
done
