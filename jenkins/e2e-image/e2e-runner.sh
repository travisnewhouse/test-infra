#!/bin/bash
# Copyright 2015 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Run e2e tests using environment variables exported in e2e.sh.

set -o errexit
set -o nounset
set -o pipefail
set -o xtrace

case "${KUBERNETES_PROVIDER}" in
    gce|gke|kubemark)
        if [[ -z "${PROJECT:-}" ]]; then
          echo "ERROR: unset PROJECT" >&2
          exit 1
        fi
        ;;
esac

# include shell2junit library
sh2ju="$(dirname "${0}")/sh2ju.sh"
if [[ -f "${sh2ju}" ]]; then
  source "${sh2ju}"
else
  echo "TODO(fejta): stop pulling sh2ju.sh"
  source <(curl -fsS --retry 3 'https://raw.githubusercontent.com/kubernetes/kubernetes/master/third_party/forked/shell2junit/sh2ju.sh')
fi

# Have cmd/e2e run by goe2e.sh generate JUnit report in ${WORKSPACE}/junit*.xml
ARTIFACTS=${WORKSPACE}/_artifacts
mkdir -p ${ARTIFACTS}

# E2E runner stages
STAGE_PRE="PRE-SETUP"
STAGE_SETUP="SETUP"
STAGE_CLEANUP="CLEANUP"
STAGE_KUBEMARK="KUBEMARK"

: ${KUBE_GCS_RELEASE_BUCKET:="kubernetes-release"}
: ${KUBE_GCS_DEV_RELEASE_BUCKET:="kubernetes-release-dev"}
JENKINS_SOAK_PREFIX="gs://kubernetes-jenkins/soak/${JOB_NAME}"

# Explicitly set config path so staging gcloud (if installed) uses same path
export CLOUDSDK_CONFIG="${WORKSPACE}/.config/gcloud"

# record_command runs the command and records its output/error messages in junit format
# it expects the first argument to be the class and the second to be the name of the command
# Example:
# record_command PRESETUP curltest curl google.com
# record_command CLEANUP check false
#
# WARNING: Variable changes in the command will NOT be effective after record_command returns.
#          This is because the command runs in subshell.
function record_command() {
    set +o xtrace
    set +o nounset
    set +o errexit

    local class=$1
    shift
    local name=$1
    shift
    echo "Recording: ${class} ${name}"
    echo "Running command: $@"
    juLog -output="${ARTIFACTS}" -class="${class}" -name="${name}" "$@"

    set -o nounset
    set -o errexit
    set -o xtrace
}

function running_in_docker() {
    grep -q docker /proc/self/cgroup
}

# Sets KUBERNETES_RELEASE and KUBERNETES_RELEASE_URL to point to tarballs in the
# local _output/gcs-stage directory.
function set_release_vars_from_local_gcs_stage() {
    local -r local_gcs_stage_path="${WORKSPACE}/_output/gcs-stage"
    KUBERNETES_RELEASE_URL="file://${local_gcs_stage_path}"
    KUBERNETES_RELEASE=$(ls "${local_gcs_stage_path}" | grep ^v.*$)
    if [[ -z "${KUBERNETES_RELEASE}" ]]; then
      echo "FAIL! version not found in ${local_gcs_stage_path}"
      return 1
    fi
}

# Use a published version like "ci/latest" (default), "release/latest",
# "release/latest-1", or "release/stable".
# TODO(ixdy): maybe this should be in get-kube.sh?
function set_release_vars_from_gcs() {
    local -r published_version="${1}"
    IFS='/' read -a varr <<< "${published_version}"
    local -r path="${varr[0]}"
    if [[ "${path}" == "release" ]]; then
      local -r bucket="${KUBE_GCS_RELEASE_BUCKET}"
    else
      local -r bucket="${KUBE_GCS_DEV_RELEASE_BUCKET}"
    fi
    KUBERNETES_RELEASE=$(gsutil cat "gs://${bucket}/${published_version}.txt")
    KUBERNETES_RELEASE_URL="https://storage.googleapis.com/${bucket}/${path}"
}

function set_release_vars_from_gke_cluster_version() {
    local -r server_version="$(gcloud ${CMD_GROUP:-} container get-server-config --project=${PROJECT} --zone=${ZONE}  --format='value(defaultClusterVersion)')"
    # Use latest build of the server version's branch for test files.
    set_release_vars_from_gcs "ci/latest-${server_version:0:3}"
}

function call_get_kube() {
    local get_kube_sh="${WORKSPACE}/get-kube.sh"
    if [[ ! -x "${get_kube_sh}" ]]; then
      # If running outside docker (e.g. in soak tests) we may not have the
      # script, so download it.
      mkdir -p "${WORKSPACE}/_tmp/"
      get_kube_sh="${WORKSPACE}/_tmp/get-kube.sh"
      curl -fsSL --retry 3 --keepalive-time 2 https://get.k8s.io/ > "${get_kube_sh}"
      chmod +x "${get_kube_sh}"
    fi
    export KUBERNETES_RELEASE
    export KUBERNETES_RELEASE_URL
    KUBERNETES_SKIP_CONFIRM=y KUBERNETES_SKIP_CREATE_CLUSTER=y KUBERNETES_DOWNLOAD_TESTS=y \
      "${get_kube_sh}"
    if [[ ! -x kubernetes/cluster/get-kube-binaries.sh ]]; then
      # If the get-kube-binaries.sh script doesn't exist, assume this is an older
      # release without it, and thus the tests haven't been downloaded yet.
      # We'll have to download and extract them ourselves instead.
      echo "Grabbing test binaries since cluster/get-kube-binaries.sh does not exist."
      local -r test_tarball=kubernetes-test.tar.gz
      curl -L "${KUBERNETES_RELEASE_URL:-https://storage.googleapis.com/kubernetes-release/release}/${KUBERNETES_RELEASE}/${test_tarball}" -o "${test_tarball}"
      md5sum "${test_tarball}"
      tar -xzf "${test_tarball}"
    fi
}

# TODO(ihmccreery) I'm not sure if this is necesssary, with the workspace check
# below.
function clean_binaries() {
    echo "Cleaning up binaries."
    rm -rf kubernetes*
}

function get_latest_docker_release() {
  # Typical Docker release versions are like v1.11.2-rc1, v1.11.2, and etc.
  local -r version_re='.*\"tag_name\":[[:space:]]+\"v([0-9\.r|c-]+)\",.*'
  local -r releases="$(curl -fsSL --retry 3 https://api.github.com/repos/docker/docker/releases)"
  # The GitHub API returns releases in descending order of creation time so the
  # first one is always the latest.
  # TODO: if we can install `jq` on the Jenkins nodes, we won't have to craft
  # regular expressions here.
  while read -r line; do
    if [[ "${line}" =~ ${version_re} ]]; then
      echo "${BASH_REMATCH[1]}"
      return
    fi
  done <<< "${releases}"
  echo "Failed to determine the latest Docker release."
  exit 1
}

function install_google_cloud_sdk_tarball() {
    local -r tarball=$1
    local -r install_dir=$2
    mkdir -p "${install_dir}"
    tar xzf "${tarball}" -C "${install_dir}"

    export CLOUDSDK_CORE_DISABLE_PROMPTS=1
    record_command "${STAGE_PRE}" "install_gcloud" "${install_dir}/google-cloud-sdk/install.sh" --disable-installation-options --bash-completion=false --path-update=false --usage-reporting=false
    export PATH=${install_dir}/google-cloud-sdk/bin:${PATH}
    gcloud components install alpha
    gcloud components install beta
    gcloud info
}

# Sets release vars using GCI image builtin k8s version.
# If JENKINS_GCI_PATCH_K8S is set, uses the latest CI build on the same branch
# instead.
# Assumes: JENKINS_GCI_HEAD_IMAGE_FAMILY and KUBE_GCE_MASTER_IMAGE
function set_release_vars_from_gci_builtin_version() {
    if ! [[ "${JENKINS_USE_GCI_VERSION:-}" =~ ^[yY]$ ]]; then
        echo "JENKINS_USE_GCI_VERSION must be set."
        exit 1
    fi
    if [[ -z "${JENKINS_GCI_HEAD_IMAGE_FAMILY:-}" ]] || [[ -z "${KUBE_GCE_MASTER_IMAGE:-}" ]]; then
        echo "JENKINS_GCI_HEAD_IMAGE_FAMILY and KUBE_GCE_MASTER_IMAGE must both be set."
        exit 1
    fi
    local -r gci_k8s_version="$(gsutil cat gs://container-vm-image-staging/k8s-version-map/${KUBE_GCE_MASTER_IMAGE})"
    if [[ "${JENKINS_GCI_PATCH_K8S}" =~ ^[yY]$ ]]; then
        # We always want to test against the builtin k8s version, but occationally
        # the builtin version has known bugs that keep our tests red. In those
        # cases, we use the latest CI build on the same branch instead.
        set_release_vars_from_gcs "ci/latest-${gci_k8s_version:0:3}"
    else
        KUBERNETES_RELEASE="v${gci_k8s_version}"
        # Use the default KUBERNETES_RELEASE_URL.
    fi
}

# Specific settings for tests that use GCI HEAD images. I.e., if your test is
# using a public/released GCI image, you don't want to call this function.
# Assumes: JENKINS_GCI_HEAD_IMAGE_FAMILY
function setup_gci_vars() {
    local -r gci_staging_project=container-vm-image-staging
    local -r image_name="$(gcloud compute images describe-from-family ${JENKINS_GCI_HEAD_IMAGE_FAMILY} --project=${gci_staging_project} --format='value(name)')"

    export KUBE_GCE_MASTER_PROJECT="${gci_staging_project}"
    export KUBE_GCE_MASTER_IMAGE="${image_name}"
    export KUBE_MASTER_OS_DISTRIBUTION="gci"

    export KUBE_GCE_NODE_PROJECT="${gci_staging_project}"
    export KUBE_GCE_NODE_IMAGE="${image_name}"
    export KUBE_NODE_OS_DISTRIBUTION="gci"

    # These will be included in started.json in the metadata dict.
    # See upload-to-gcs.sh for more details.
    export BUILD_METADATA_GCE_MASTER_IMAGE="${KUBE_GCE_MASTER_IMAGE}"
    export BUILD_METADATA_GCE_NODE_IMAGE="${KUBE_GCE_NODE_IMAGE}"

    # For backward compatibility (Older versions of Kubernetes don't understand
    # KUBE_MASTER_OS_DISTRIBUTION or KUBE_NODE_OS_DISTRIBUTION. Only KUBE_OS_DISTRIBUTION can be
    # used for them.)
    export KUBE_OS_DISTRIBUTION="gci"

    if [[ "${JENKINS_GCI_HEAD_IMAGE_FAMILY}" == "gci-canary-test" ]]; then
        # The family "gci-canary-test" is reserved for a special type of GCI images
        # that are used to continuously validate Docker releases.
        export KUBE_GCI_DOCKER_VERSION="$(get_latest_docker_release)"
    fi
}

### Pre Set Up ###
if running_in_docker; then
    record_command "${STAGE_PRE}" "download_gcloud" curl -fsSL --retry 3 --keepalive-time 2 -o "${WORKSPACE}/google-cloud-sdk.tar.gz" 'https://dl.google.com/dl/cloudsdk/channels/rapid/google-cloud-sdk.tar.gz'
    install_google_cloud_sdk_tarball "${WORKSPACE}/google-cloud-sdk.tar.gz" /
    if [[ "${KUBERNETES_PROVIDER}" == 'aws' ]]; then
        pip install awscli
    fi
fi

if [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]]; then
  gcloud auth activate-service-account --key-file="${GOOGLE_APPLICATION_CREDENTIALS}"
fi

# Install gcloud from a custom path if provided. Used to test GKE with gcloud
# at HEAD, release candidate.
# TODO: figure out how to avoid installing the cloud sdk twice if run inside Docker.
if [[ -n "${CLOUDSDK_BUCKET:-}" ]]; then
    # Retry the download a few times to mitigate transient server errors and
    # race conditions where the bucket contents change under us as we download.
    for n in {1..3}; do
        gsutil -mq cp -r "${CLOUDSDK_BUCKET}" ~ && break || sleep 1
        # Delete any temporary files from the download so that we start from
        # scratch when we retry.
        rm -rf ~/.gsutil
    done
    rm -rf ~/repo ~/cloudsdk
    mv ~/$(basename "${CLOUDSDK_BUCKET}") ~/repo
    export CLOUDSDK_COMPONENT_MANAGER_SNAPSHOT_URL=file://${HOME}/repo/components-2.json
    install_google_cloud_sdk_tarball ~/repo/google-cloud-sdk.tar.gz ~/cloudsdk

    # Just in case the new gcloud stores credentials differently, re-activate
    # credentials.
    if [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]]; then
      gcloud auth activate-service-account --key-file="${GOOGLE_APPLICATION_CREDENTIALS}"
    fi
fi

# Specific settings for tests that use GCI HEAD images. I.e., if your test is
# using a public/released GCI image, you don't want to set this variable or call
# `setup_gci_vars`.
if [[ -n "${JENKINS_GCI_HEAD_IMAGE_FAMILY:-}" ]]; then
  setup_gci_vars
fi

echo "--------------------------------------------------------------------------------"
echo "Test Environment:"
printenv | sort
echo "--------------------------------------------------------------------------------"

# Set this var instead of exiting-- we must do the cluster teardown step. We'll
# return this at the very end.
EXIT_CODE=0

# We get the Kubernetes tarballs unless we are going to use old ones
if [[ "${JENKINS_USE_EXISTING_BINARIES:-}" =~ ^[yY]$ ]]; then
  echo "Using existing binaries; not cleaning, fetching, or unpacking new ones."
else
  clean_binaries

  if [[ "${JENKINS_SOAK_MODE:-}" == "y" && ${E2E_UP:-} != "true" ]]; then
    # We are restoring the cluster, copy state from gcs
    # TODO(fejta): auto-detect and recover from deployment failures
    mkdir -p "${HOME}/.kube"
    gsutil cp "${JENKINS_SOAK_PREFIX}/kube-config" "${HOME}/.kube/config"
    export KUBERNETES_RELEASE=$(gsutil cat "${JENKINS_SOAK_PREFIX}/release.txt")
    export KUBERNETES_RELEASE_URL=$(gsutil cat "${JENKINS_SOAK_PREFIX}/release-url.txt")
    export CLUSTER_API_VERSION=$(echo "${KUBERNETES_RELEASE}" | cut -c 2-)  # for GKE CI
  elif [[ "${JENKINS_USE_LOCAL_BINARIES:-}" =~ ^[yY]$ ]]; then
    set_release_vars_from_local_gcs_stage
  elif [[ "${JENKINS_USE_SERVER_VERSION:-}" =~ ^[yY]$ ]]; then
    # This is for test, staging, and prod jobs on GKE, where we want to
    # test what's running in GKE by default rather than some CI build.
    set_release_vars_from_gke_cluster_version
  elif [[ "${JENKINS_USE_GCI_VERSION:-}" =~ ^[yY]$ ]]; then
    # Use GCI image builtin version. Needed for GCI release qual tests.
    set_release_vars_from_gci_builtin_version
  else
    # use JENKINS_PUBLISHED_VERSION, default to 'ci/latest', since that's
    # usually what we're testing.
    set_release_vars_from_gcs "${JENKINS_PUBLISHED_VERSION:-ci/latest}"
    # Needed for GKE CI.
    export CLUSTER_API_VERSION=$(echo "${KUBERNETES_RELEASE}" | cut -c 2-)
  fi

  call_get_kube
fi

# Copy GCE keys so we don't keep cycling them.
# To set this up, you must know the <project>, <zone>, and <instance>
# on which your jenkins jobs are running. Then do:
#
# # SSH from your computer into the instance.
# $ gcloud compute ssh --project="<prj>" ssh --zone="<zone>" <instance>
#
# # Generate a key by ssh'ing from the instance into itself, then exit.
# $ gcloud compute ssh --project="<prj>" ssh --zone="<zone>" <instance>
# $ ^D
#
# # Copy the keys to the desired location (e.g. /var/lib/jenkins/gce_keys/).
# $ sudo mkdir -p /var/lib/jenkins/gce_keys/
# $ sudo cp ~/.ssh/google_compute_engine /var/lib/jenkins/gce_keys/
# $ sudo cp ~/.ssh/google_compute_engine.pub /var/lib/jenkins/gce_keys/
#
# # Move the permissions for the keys to Jenkins.
# $ sudo chown -R jenkins /var/lib/jenkins/gce_keys/
# $ sudo chgrp -R jenkins /var/lib/jenkins/gce_keys/
case "${KUBERNETES_PROVIDER}" in
    gce|gke|kubemark)
        if ! running_in_docker; then
            mkdir -p ${WORKSPACE}/.ssh/
            cp /var/lib/jenkins/gce_keys/google_compute_engine ${WORKSPACE}/.ssh/
            cp /var/lib/jenkins/gce_keys/google_compute_engine.pub ${WORKSPACE}/.ssh/
        fi
        echo 'Checking existence of private ssh key'
        gce_key="${WORKSPACE}/.ssh/google_compute_engine"
        if [[ ! -f "${gce_key}" || ! -f "${gce_key}.pub" ]]; then
            echo 'google_compute_engine ssh key missing!'
            exit 1
        fi
        echo "Checking presence of public key in ${PROJECT}"
        if ! gcloud compute --project="${PROJECT}" project-info describe |
             grep "$(cat "${gce_key}.pub")" >/dev/null; then
            echo 'Uploading public ssh key to project metadata...'
            gcloud compute --project="${PROJECT}" config-ssh
        fi
        ;;
    default)
        echo "Not copying ssh keys for ${KUBERNETES_PROVIDER}"
        ;;
esac


# Allow download & unpack of alternate version of tests, for cross-version & upgrade testing.
#
# JENKINS_PUBLISHED_SKEW_VERSION downloads an alternate version of Kubernetes
# for testing, moving the old one to kubernetes_old.
#
# E2E_UPGRADE_TEST=true triggers a run of the e2e tests, to do something like
# upgrade the cluster, before the main test run.  It uses
# GINKGO_UPGRADE_TESTS_ARGS for the test run.
#
# JENKINS_USE_SKEW_TESTS=true will run tests from the skewed version rather
# than the original version.
if [[ -n "${JENKINS_PUBLISHED_SKEW_VERSION:-}" ]]; then
  mv kubernetes kubernetes_orig
  (
      # Subshell so we don't override KUBERNETES_RELEASE
      set_release_vars_from_gcs "${JENKINS_PUBLISHED_SKEW_VERSION}"
      call_get_kube
  )
  mv kubernetes kubernetes_skew
  mv kubernetes_orig kubernetes
  export BUILD_METADATA_KUBERNETES_SKEW_VERSION=$(cat kubernetes_skew/version || true)
  if [[ "${JENKINS_USE_SKEW_TESTS:-}" != "true" ]]; then
    # Append kubectl-path of skewed kubectl to test args, since we always
    # want that to use the skewed kubectl version:
    #  - for upgrade jobs, we want kubectl to be at the same version as
    #    master.
    #  - for client skew tests, we want to use the skewed kubectl
    #    (that's what we're testing).
    GINKGO_TEST_ARGS="${GINKGO_TEST_ARGS:-} --kubectl-path=$(pwd)/kubernetes_skew/cluster/kubectl.sh"
  fi
fi

cd kubernetes

if [[ -n "${BOOTSTRAP_MIGRATION:-}" ]]; then
  # TODO(fejta): migrate all jobs and stop using upload-to-gcs.sh to do this
  # Right now started.json is created by e2e-runner.sh and
  # finished.json is created by jenkins.
  # Soon we will consolodate this responsibility inside bootstrap.py
  # We want to switch to this logic as we start migrating jobs over to this
  # pattern, but until then we also need jobs to continue uploading started.json
  # until that time. This environment variable will do that.
  source "$(dirname "${0}")/upload-to-gcs.sh"
  version=$(find_version)  # required by print_started
  print_started | jq '.metadata? + {version, "job-version"}' > "${ARTIFACTS}/metadata.json"
elif [[ ! "${JOB_NAME}" =~ -pull- ]]; then
  echo "The bootstrapper should handle Tracking the start/finish of a job and "
  echo "uploading artifacts. TODO(fejta): migrate this job"
  # Upload build start time and k8s version to GCS, but not on PR Jenkins.
  # On PR Jenkins this is done before the build.
  upload_to_gcs="$(dirname "${0}")/upload-to-gcs.sh"
  if [[ -f "${upload_to_gcs}" ]]; then
    JENKINS_BUILD_STARTED=true "${upload_to_gcs}"
  else
    echo "TODO(fejta): stop pulling upload-to-gcs.sh"
    JENKINS_BUILD_STARTED=true bash <(curl -fsS --retry 3 --keepalive-time 2 "https://raw.githubusercontent.com/kubernetes/kubernetes/master/hack/jenkins/upload-to-gcs.sh")
  fi
fi

# When run inside Docker, we need to make sure all files are world-readable
# (since they will be owned by root on the host).
trap "chmod -R o+r '${ARTIFACTS}'" EXIT SIGINT SIGTERM
export E2E_REPORT_DIR=${ARTIFACTS}

e2e_go_args=( \
  -v \
  --dump="${ARTIFACTS}" \
)


if [[ "${FAIL_ON_GCP_RESOURCE_LEAK:-true}" == "true" ]]; then
  case "${KUBERNETES_PROVIDER}" in
    gce|gke)
      e2e_go_args+=(--check_leaked_resources)
      ;;
  esac
fi

if [[ "${E2E_UP:-}" == "true" ]]; then
  e2e_go_args+=(--up --ctl="version --match-server-version=false")
fi

if [[ "${E2E_DOWN:-}" == "true" ]]; then
  e2e_go_args+=(--down)
fi

if [[ "${E2E_TEST:-}" == "true" ]]; then
  e2e_go_args+=(--test)
  if [[ -n "${GINKGO_TEST_ARGS:-}" ]]; then
    e2e_go_args+=(--test_args="${GINKGO_TEST_ARGS}")
  fi
fi

# Optionally run tests from the version in  kubernetes_skew
if [[ "${JENKINS_USE_SKEW_TESTS:-}" == "true" ]]; then
  e2e_go_args+=(--skew)
fi

# Optionally run upgrade tests before other tests.
if [[ "${E2E_UPGRADE_TEST:-}" == "true" ]]; then
  e2e_go_args+=(--upgrade_args="${GINKGO_UPGRADE_TEST_ARGS}")
fi

if [[ "${USE_KUBEMARK:-}" == "true" ]]; then
  e2e_go_args+=("--kubemark=true")
fi

if [[ -n "${KUBEKINS_TIMEOUT:-}" ]]; then
  e2e_go_args+=("--timeout=${KUBEKINS_TIMEOUT}")
fi

e2e_go="$(dirname "${0}")/e2e.go"
if [[ ! -f "${e2e_go}" || -e "./hack/jenkins/.use_head_e2e" ]]; then
  echo "Using HEAD version of e2e.go."
  e2e_go="./hack/e2e.go"
fi
go run "${e2e_go}" ${E2E_OPT:-} "${e2e_go_args[@]}"

if [[ "${E2E_PUBLISH_GREEN_VERSION:-}" == "true" ]]; then
  # Use plaintext version file packaged with kubernetes.tar.gz
  echo "Publish version to ci/latest-green.txt: $(cat version)"
  gsutil cp ./version "gs://${KUBE_GCS_DEV_RELEASE_BUCKET}/ci/latest-green.txt"
fi

if [[ "${JENKINS_SOAK_MODE:-}" == "y" && ${E2E_UP:-} == "true" ]]; then
  # We deployed a cluster, save state to gcs
  gsutil cp -a project-private "${HOME}/.kube/config" "${JENKINS_SOAK_PREFIX}/kube-config"
  echo ${KUBERNETES_RELEASE} | gsutil cp - "${JENKINS_SOAK_PREFIX}/release.txt"
  echo ${KUBERNETES_RELEASE_URL} | gsutil cp - "${JENKINS_SOAK_PREFIX}/release-url.txt"
fi
