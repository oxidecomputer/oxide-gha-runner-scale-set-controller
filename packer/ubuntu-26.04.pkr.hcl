packer {
  required_plugins {
    oxide = {
      source  = "github.com/oxidecomputer/oxide"
      version = "~> 0.0"
    }
  }
}

variable "source_image_name" {
  type        = string
  description = "Name of the Ubuntu 26.04 image to build from."
}

variable "source_image_project" {
  type        = string
  description = "Project containing the source image, or null for a silo image."
  default     = null
}

variable "runner_version" {
  type        = string
  description = "The GitHub Actions runner version to install."
}

variable "runner_sha256sum" {
  type        = string
  description = "The SHA-256 checksum of the GitHub Actions runner archive."
}

variable "project" {
  type        = string
  description = "Project in which to create the build instance."
}

variable "boot_disk_size" {
  type        = number
  description = "Build instance boot disk size in bytes."
  default     = 8 * 1024 * 1024 * 1024
}

variable "cpus" {
  type        = number
  description = "Number of CPUs for the build instance."
  default     = 2
}

variable "memory" {
  type        = number
  description = "Build instance memory in bytes."
  default     = 8 * 1024 * 1024 * 1024
}

variable "vpc" {
  type        = string
  description = "VPC for the build instance."
  default     = "default"
}

variable "subnet" {
  type        = string
  description = "Subnet for the build instance."
  default     = "default"
}

variable "ip_pool" {
  type        = string
  description = "External IP pool for the build instance, or null for the default."
  default     = null
}

variable "artifact_name" {
  type        = string
  description = "Name of the output image."
  default     = "oxide-gha-runner"
}

variable "ssh_username" {
  type        = string
  description = "SSH user in the source image."
  default     = "ubuntu"
}

data "oxide-image" "source" {
  name    = var.source_image_name
  project = var.source_image_project
}

source "oxide-instance" "gha-runner" {
  project            = var.project
  boot_disk_image_id = data.oxide-image.source.image_id
  boot_disk_size     = var.boot_disk_size
  cpus               = var.cpus
  memory             = var.memory
  ip_pool            = var.ip_pool
  vpc                = var.vpc
  subnet             = var.subnet

  communicator = "ssh"
  ssh_username = var.ssh_username
  ssh_timeout  = "15m"

  artifact_name = var.artifact_name
}

build {
  sources = ["source.oxide-instance.gha-runner"]

  # Let the user-data execution settle before proceeding.
  provisioner "shell" {
    inline          = ["cloud-init status --wait"]
    execute_command = "sudo env {{ .Vars }} bash '{{ .Path }}'"
  }

  provisioner "file" {
    source      = "${path.root}/files/github-actions-runner.service"
    destination = "/tmp/github-actions-runner.service"
  }

  provisioner "file" {
    source      = "${path.root}/files/github-actions-runner.path"
    destination = "/tmp/github-actions-runner.path"
  }

  provisioner "file" {
    source      = "${path.root}/files/github-actions-runner.tmpfiles"
    destination = "/tmp/github-actions-runner.tmpfiles"
  }

  provisioner "file" {
    source      = "${path.root}/files/github-actions-runner"
    destination = "/tmp/github-actions-runner"
  }

  provisioner "shell" {
    environment_vars = [
      "DEBIAN_FRONTEND=noninteractive",
      "NEEDRESTART_MODE=l",
      "GITHUB_ACTIONS_RUNNER_VERSION=${var.runner_version}",
      "GITHUB_ACTIONS_RUNNER_SHA256SUM=${var.runner_sha256sum}",
      "GITHUB_ACTIONS_RUNNER_SERVICE_SOURCE=/tmp/github-actions-runner.service",
      "GITHUB_ACTIONS_RUNNER_PATH_SOURCE=/tmp/github-actions-runner.path",
      "GITHUB_ACTIONS_RUNNER_TMPFILES_SOURCE=/tmp/github-actions-runner.tmpfiles",
      "GITHUB_ACTIONS_RUNNER_LAUNCHER_SOURCE=/tmp/github-actions-runner",
    ]
    scripts = [
      "${path.root}/scripts/update-os.sh",
      "${path.root}/scripts/install-packages.sh",
      "${path.root}/scripts/install-github-actions-runner.sh",
    ]
    execute_command = "sudo env {{ .Vars }} bash '{{ .Path }}'"
  }

  # This must remain the final provisioner. It removes all SSH keys, resets
  # instance identity, and flushes the filesystem.
  provisioner "shell" {
    environment_vars = ["BUILD_USER=${var.ssh_username}"]
    script           = "${path.root}/scripts/finalize-image.sh"
    execute_command  = "sudo env {{ .Vars }} bash '{{ .Path }}'"
  }
}
