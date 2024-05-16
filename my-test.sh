set -ex
make kind-init
make kind-install
make ovn-vpc-nat-gw-conformance-e2e 2>&1 | tee test.log

