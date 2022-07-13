#!/bin/bash
set -e

# Get SHA of ubuntu 18.04 from /universe/docker-images/ubuntu/18.04/BUILD before switching to ratelimit dir
# Extract sha value from string like blow
# sha = "sha256:d91a3b36f547aa1db80d9d576e19d177206c3a94791ac51c26d40d246a96fd2b",
UBUNTU_BUILD_FILE_LOCATION="~/universe/docker-images/ubuntu/18.04/BUILD"
if [ ! -f "$UBUNTU_BUILD_FILE_LOCATION" ]; then
    UBUNTU_BUILD_FILE_LOCATION="./universe/docker-images/ubuntu/18.04/BUILD"
fi
IMAGE_SHA="$(< "$UBUNTU_BUILD_FILE_LOCATION" grep sha | awk 'NR==1{print substr($3, 2, length($3)-3)}' )"
echo "Ubuntu 18.04 SHA256: $IMAGE_SHA"

export REGISTRY_LOCATION='registry.dev.databricks.com/m3/m3coordinator-base-vcache'
DOCKER_COMMAND="docker build -f docker/m3coordinator/Dockerfile --build-arg IMAGE_SHA=${IMAGE_SHA} -t \"$REGISTRY_LOCATION\":temporary ."
echo "Running: " "$DOCKER_COMMAND"
bash -c "$DOCKER_COMMAND"
# TROUBLESHOOTING: if this fails, check `bin/get-kube-access dev` output and run any symlink
# commands output. Note that the symlink target may not be what docker push wants and you may need
# to change it.
OUTPUT="$(docker push "$REGISTRY_LOCATION:temporary")"
DIGEST="$(echo "$OUTPUT" | grep digest | awk '{print $3}' | sort -u)"
echo "Image: $REGISTRY_LOCATION"
echo "Digest: $DIGEST"
