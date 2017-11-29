#!/bin/bash -eux

export BOSH_DOCKER_CPI_RELEASE_REPO=$PWD/bosh-docker-cpi-release
export BOSH_DEPLOYMENT_REPO=$PWD/bosh-deployment

export BBL_STATE_DIR=$PWD/bbl-state/${BBL_STATE_SUBDIRECTORY}
export STATE_DIR=$(mktemp -d)

tasks_directory=$(dirname $0)
scripts_directory=${tasks_directory}/test-stress
cd ${scripts_directory}

eval "$(bbl print-env --state-dir=$BBL_STATE_DIR)"

deployments="${DEPLOYMENTS_OF_100:=10}"

# 1. Deploy docker hosts to outer director
pushd docker-hosts-deployment
  ./deploy-docker.sh
popd

# 2. Deploy innr director
pushd inner-bosh-deployment
  ./deploy-director.sh
popd

# target inner bosh
export BOSH_CA_CERT="$(bosh int ${STATE_DIR}/vars-store.yml --path /director_ssl/ca)"
export BOSH_CLIENT="admin"
export BOSH_CLIENT_SECRET="$(bosh int ${STATE_DIR}/vars-store.yml --path /admin_password)"
export BOSH_ENVIRONMENT="https://$(bosh int inner-bosh-deployment/vars.yml --path /internal_ip):25555"

  pushd inner-bosh-workspace
    # 3. 10x Deploy large bosh-dns deployment to inner director
    ./deploy-n.sh $deployments

    # 4. Run test
    ./check-dns.sh $deployments
  popd

# retarget outer bosh
eval "$(bbl print-env --state-dir=$BBL_STATE_DIR)"

# skip teardown of docker container vms since they'll be torn down with the host

# 5. Delete inner director
bosh -n -d bosh delete-deployment

# 6. Delete docker hosts from outer director
bosh -n -d docker delete-deployment

# 7. Clean-up old artifacts
bosh -n clean-up --all
