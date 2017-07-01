#!/bin/bash

set -e -o pipefail
set -x

function realpath() {
    [[ $1 = /* ]] && echo "$1" || echo "$PWD/${1#./}"
}

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

fly -t production login

WORK_DIR=/tmp/test-acceptance-windows-workdir
echo "working out of ${WORK_DIR}..."
mkdir -p $WORK_DIR
cd $WORK_DIR

if [ ! -e bbl.env ]; then
    # force lpass logged in
    lpass show xxxxxxxxxxxxxxxxxxxxxx 2>/dev/null
    cat <<EOF > bbl.env
export BBL_GCP_SERVICE_ACCOUNT_KEY='$(lpass show 3654688481222762882 --notes | gobosh int - --path /bbl_gcp_service_account_key_id)'
export BBL_GCP_PROJECT_ID=cf-bosh-core
export BBL_GCP_ZONE=us-central1-a
export BBL_GCP_REGION=us-central1
export BBL_IAAS=gcp
EOF
fi

# Download bosh-cli if it doesn't exist
BOSH_CLI_DIR=${WORK_DIR}/bosh-cli
mkdir -p $BOSH_CLI_DIR
if [ ! -e $BOSH_CLI_DIR/bosh-cli-linux ]; then
    pushd ${BOSH_CLI_DIR}
      curl -o bosh-cli-linux https://s3.amazonaws.com/bosh-cli-alpha-artifacts/alpha-bosh-cli-0.0.250-linux-amd64
    popd
fi

# Download the bbl cli if it doesn't exist
BBL_CLI_DIR=${WORK_DIR}/bbl-cli
mkdir -p $BBL_CLI_DIR
if [ ! -e ${BBL_CLI_DIR}/bbl-v3.2.0_linux_x86-64 ]; then
    pushd $BBL_CLI_DIR
      curl -o bbl-v3.2.0_linux_x86-64 -L -v https://github.com/cloudfoundry/bosh-bootloader/releases/download/v3.2.0/bbl-v3.2.0_linux_x86-64
      chmod +x ./bbl-v3.2.0_linux_x86-64
    popd
fi

# Download bosh-release if it doesn't exist
BOSH_RELEASE_DIR=${WORK_DIR}/bosh-release
mkdir -p $BOSH_RELEASE_DIR
if [ ! -e ${BOSH_RELEASE_DIR}/bosh-dev-release.tgz ]; then
    pushd $BOSH_RELEASE_DIR
      # For latest 262.1, use: https://bosh.io/d/github.com/cloudfoundry/bosh?v=262.1
      curl -L -J -o bosh-dev-release.tgz https://s3.amazonaws.com/bosh-compiled-release-tarballs/bosh-262.1-ubuntu-trusty-3421.9-20170621-055124-244370454-20170621055129.tgz?versionId=lxNGZVeHOlvxh4LyMgNxnHC8wczKDP70
    popd
fi

BBL_STATE_DIR=$WORK_DIR/bbl-state
mkdir -p $BBL_STATE_DIR
pushd $BBL_STATE_DIR
  set +e
  git init
  set -e
popd

docker pull bosh/main-ruby-go

docker run \
  -t -i \
  -v $BBL_STATE_DIR:'/bbl-state' \
  -v `realpath $BOSH_RELEASE_DIR`:/bosh-candidate-release \
  -v `realpath $BBL_CLI_DIR`:/bbl-cli \
  -v `realpath $BOSH_CLI_DIR`:/bosh-cli \
  -v ~/workspace/bosh-deployment:/bosh-deployment \
  -v $DIR/..:/dns-release \
  bosh/main-ruby-go \
  bash -c 'source /dns-release/bbl.env; /dns-release/ci/tasks/bbl-up.sh'

# windows specfic
fly -t production execute -x --privileged \
  --config=$DIR/../ci/tasks/test-acceptance-windows.yml \
  --inputs-from=dns-release/test-acceptance-windows \
  --input=dns-release=$DIR/../ \
  --input=bbl-state=$BBL_STATE_DIR

# shared
fly -t production execute -x --privileged \
    --inputs-from=dns-release/test-acceptance-windows \
    --input=dns-release=$DIR/../ \
    --input=bbl-state=$BBL_STATE_DIR \
    --config=$DIR/../ci/tasks/test-acceptance-windows-shared.yml

docker run \
  -t -i \
  -v $BBL_STATE_DIR:'/bbl-state' \
  -v `realpath $BBL_CLI_DIR`:/bbl-cli \
  -v `realpath $BOSH_CLI_DIR`:/bosh-cli \
  -v ~/workspace/bosh-deployment:/bosh-deployment \
  -v $DIR/..:/dns-release bosh/main-ruby-go \
  bash -c 'source /dns-release/bbl.env; /dns-release/ci/tasks/bbl-destroy.sh'

# Clean up creds.yml so that the next bbl-up will recreate certs for the new instances.
rm -f $BBL_STATE_DIR/creds.yml

exit 0