#!/usr/bin/env bash

set -ex

source lib.sh

main() {
    setup-env
    build-provisioner
}


main