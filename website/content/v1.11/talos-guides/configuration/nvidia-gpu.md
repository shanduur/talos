---
title: "NVIDIA GPU (OSS drivers)"
description: "In this guide we'll follow the procedure to support NVIDIA GPU using OSS drivers on Talos."
aliases:
  - ../../guides/nvidia-gpu
---

> Enabling NVIDIA GPU support on Talos is bound by [NVIDIA EULA](https://www.nvidia.com/en-us/drivers/nvidia-license/).
> The Talos published NVIDIA OSS drivers are bound to a specific Talos release.
> The extensions versions also needs to be updated when upgrading Talos.

We will be using the following NVIDIA OSS system extensions:

- `nvidia-open-gpu-kernel-modules`
- `nvidia-container-toolkit`

Create the [boot assets]({{< relref "../install/boot-assets" >}}) which includes the system extensions mentioned above (or create a custom installer and perform a machine upgrade if Talos is already installed).

> Make sure the driver version matches for both the `nvidia-open-gpu-kernel-modules` and `nvidia-container-toolkit` extensions.
> The `nvidia-open-gpu-kernel-modules` extension is versioned as `<nvidia-driver-version>-<talos-release-version>` and the `nvidia-container-toolkit` extension is versioned as `<nvidia-driver-version>-<nvidia-container-toolkit-version>`.

## Proprietary vs OSS Nvidia Driver Support

The NVIDIA Linux GPU Driver contains several kernel modules: `nvidia.ko`, `nvidia-modeset.ko`, `nvidia-uvm.ko`, `nvidia-drm.ko`, and `nvidia-peermem.ko`.
Two "flavors" of these kernel modules are provided, and both are available for use within Talos:

- Proprietary, This is the flavor that NVIDIA has historically shipped.
- Open, i.e. source-published/OSS, kernel modules that are dual licensed MIT/GPLv2.
  With every driver release, the source code to the open kernel modules is published on https://github.com/NVIDIA/open-gpu-kernel-modules and a tarball is provided on https://download.nvidia.com/XFree86/.

The choice between Proprietary/OSS may be decided after referencing the Official [NVIDIA announcement](https://developer.nvidia.com/blog/nvidia-transitions-fully-towards-open-source-gpu-kernel-modules/).

## Enabling the NVIDIA OSS modules

Patch Talos machine configuration using the patch `gpu-worker-patch.yaml`:

```yaml
machine:
  kernel:
    modules:
      - name: nvidia
      - name: nvidia_uvm
      - name: nvidia_drm
      - name: nvidia_modeset
  sysctls:
    net.core.bpf_jit_harden: 1
```

Now apply the patch to all Talos nodes in the cluster having NVIDIA GPU's installed:

```bash
talosctl patch mc --patch @gpu-worker-patch.yaml
```

The NVIDIA modules should be loaded and the system extension should be installed.

This can be confirmed by running:

```bash
talosctl get modules
```

which should produce an output similar to below:

```text
NODE       NAMESPACE   TYPE                 ID                     VERSION   STATE
10.5.0.3   runtime     LoadedKernelModule   nvidia_uvm             1         Live
10.5.0.3   runtime     LoadedKernelModule   nvidia_drm             1         Live
10.5.0.3   runtime     LoadedKernelModule   nvidia_modeset         1         Live
10.5.0.3   runtime     LoadedKernelModule   nvidia                 1         Live
```

```bash
talosctl get extensions
```

which should produce an output similar to below:

```text
NODE           NAMESPACE   TYPE              ID                                                                           VERSION   NAME                             VERSION
172.31.41.27   runtime     ExtensionStatus   000.ghcr.io-siderolabs-nvidia-container-toolkit-515.65.01-v1.10.0            1         nvidia-container-toolkit         515.65.01-v1.10.0
172.31.41.27   runtime     ExtensionStatus   000.ghcr.io-siderolabs-nvidia-open-gpu-kernel-modules-515.65.01-v1.2.0       1         nvidia-open-gpu-kernel-modules   515.65.01-v1.2.0
```

## Deploying NVIDIA device plugin

First we need to create the `RuntimeClass`

Apply the following manifest to create a runtime class that uses the extension:

```yaml
---
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: nvidia
```

Install the NVIDIA device plugin:

```bash
helm repo add nvdp https://nvidia.github.io/k8s-device-plugin
helm repo update
helm install nvidia-device-plugin nvdp/nvidia-device-plugin --version=0.13.0 --set=runtimeClassName=nvidia
```

## (Optional) Setting the default runtime class as `nvidia`

> Do note that this will set the default runtime class to `nvidia` for all pods scheduled on the node.

Create a patch yaml `nvidia-default-runtimeclass.yaml` to update the machine config similar to below:

```yaml
- op: add
  path: /machine/files
  value:
    - content: |
        [plugins]
          [plugins."io.containerd.cri.v1.runtime"]
            [plugins."io.containerd.cri.v1.runtime".containerd]
              default_runtime_name = "nvidia"
      path: /etc/cri/conf.d/20-customization.part
      op: create
```

Now apply the patch to all Talos nodes in the cluster having NVIDIA GPU's installed:

```bash
talosctl patch mc --patch @nvidia-default-runtimeclass.yaml
```

### Testing the runtime class

> Note the `spec.runtimeClassName` being explicitly set to `nvidia` in the pod spec.

Run the following command to test the runtime class:

```bash
kubectl run \
  nvidia-test \
  --restart=Never \
  -ti --rm \
  --image nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0 \
  --overrides '{"spec": {"runtimeClassName": "nvidia"}}' \
  nvidia-smi
```
