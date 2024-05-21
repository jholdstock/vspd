#!/usr/bin/env bash
#
# Copyright (c) 2020-2023 The Decred developers
# Use of this source code is governed by an ISC
# license that can be found in the LICENSE file.
#
# Tmux script that sets up a testnet vspd deployment with multiple voting wallets.
#
# To use the script simply run `./harness.sh` from the repo root.
#
# By default, the harness script will use `/tmp/vspd-harness` as a working
# directory. This can be changed using the `-r` flag, eg:
#
#   ./harness.sh -r /my/harness/path
#
# The harness script makes a few assumptions about the system it is running on:
# - tmux is installed
# - dcrd, dcrwallet and vspd are available on $PATH
# - Decred testnet chain is already downloaded and sync'd
# - dcrd transaction index is already built
# - The following files exist:
#   - ${HOME}/.dcrd/rpc.cert
#   - ${HOME}/.dcrd/rpc.key
#   - ${HOME}/.dcrwallet/rpc.cert
#   - ${HOME}/.dcrwallet/rpc.key

set -e

TMUX_SESSION="vspd-harness"
RPC_USER="user"
RPC_PASS="pass"
NUMBER_OF_WALLETS=3

DCRD_RPC_CERT="${HOME}/.dcrd/rpc.cert"
DCRD_RPC_KEY="${HOME}/.dcrd/rpc.key"

WALLET_PASS="12345"
WALLET_RPC_CERT="${HOME}/.dcrwallet/rpc.cert"
WALLET_RPC_KEY="${HOME}/.dcrwallet/rpc.key"

VSPD_FEE_XPUB="tpubVppjaMjp8GEWzpMGHdXNhkjqof8baKGkUzneNEiocnnjnjY9hQPe6mxzZQyzyKYS3u5yxLp8KrJvibqDzc75RGqzkv2JMPYDXmCRR1a39jg"

HARNESS_ROOT=/tmp/vspd-harness
while getopts r: flag
do
    case "${flag}" in
        r) HARNESS_ROOT=${OPTARG};
    esac
done

if [ -d "${HARNESS_ROOT}" ]; then
  while true; do
    read -p "Wipe existing harness dir? " yn
    case $yn in
        
        [Yy]* ) rm -R "${HARNESS_ROOT}"; break;;
        [Nn]* ) break;;
        * ) echo "Please answer yes or no.";;
    esac
  done
fi

tmux new-session -d -s $TMUX_SESSION

#################################################
# Setup dcrd.
#################################################

tmux rename-window -t $TMUX_SESSION 'dcrd'

echo "Writing config for dcrd"
mkdir -p "${HARNESS_ROOT}/dcrd"
cat > "${HARNESS_ROOT}/dcrd/dcrd.conf" <<EOF
rpcuser=${RPC_USER}
rpcpass=${RPC_PASS}
rpccert=${DCRD_RPC_CERT}
rpckey=${DCRD_RPC_KEY}
logdir=${HARNESS_ROOT}/dcrd/log
testnet=true
debuglevel=info
txindex=true
EOF

echo "Starting dcrd"
tmux send-keys "dcrd -C ${HARNESS_ROOT}/dcrd/dcrd.conf" C-m 

sleep 1 # Give dcrd time to start

#################################################
# Setup multiple dcrwallets.
#################################################

for ((i = 1; i <= $NUMBER_OF_WALLETS; i++)); do
    WALLET_RPC_LISTEN="127.0.0.1:2011${i}"

    # Concatenate all wallet details for vspd config file.
    if [ $i == 1 ]; then
        ALL_WALLET_HOST="${WALLET_RPC_LISTEN}"
        ALL_WALLET_USER="${RPC_USER}"
        ALL_WALLET_PASS="${RPC_PASS}"
        ALL_WALLET_CERT="${WALLET_RPC_CERT}"
    else
        ALL_WALLET_HOST="${ALL_WALLET_HOST},${WALLET_RPC_LISTEN}"
        ALL_WALLET_USER="${ALL_WALLET_USER},${RPC_USER}"
        ALL_WALLET_PASS="${ALL_WALLET_PASS},${RPC_PASS}"
        ALL_WALLET_CERT="${ALL_WALLET_CERT},${WALLET_RPC_CERT}"
    fi


echo ""
echo "Writing config for dcrwallet-${i}"
mkdir -p "${HARNESS_ROOT}/dcrwallet-${i}"
cat > "${HARNESS_ROOT}/dcrwallet-${i}/dcrwallet.conf" <<EOF
username=${RPC_USER}
password=${RPC_PASS}
rpccert=${WALLET_RPC_CERT}
rpckey=${WALLET_RPC_KEY}
logdir=${HARNESS_ROOT}/dcrwallet-${i}/log
appdata=${HARNESS_ROOT}/dcrwallet-${i}
pass=${WALLET_PASS}
grpclisten=127.0.0.1:2010${i}
rpclisten=${WALLET_RPC_LISTEN}
enablevoting=true
manualtickets=true
testnet=true
debuglevel=info
EOF

echo "Starting dcrwallet-${i}"
tmux new-window -t $TMUX_SESSION -n "dcrwallet-${i}"
# Create wallet.
tmux send-keys "dcrwallet -C ${HARNESS_ROOT}/dcrwallet-${i}/dcrwallet.conf --create <<EOF
y
n
n
ok
EOF" C-m
sleep 1
# Start wallet.
tmux send-keys "dcrwallet -C ${HARNESS_ROOT}/dcrwallet-${i}/dcrwallet.conf " C-m

done

#################################################
# Setup vspd.
#################################################

echo ""
echo "Writing config for vspd"
mkdir -p "${HARNESS_ROOT}/vspd"
cat > "${HARNESS_ROOT}/vspd/vspd.conf" <<EOF
dcrduser = ${RPC_USER}
dcrdpass = ${RPC_PASS}
dcrdcert = ${DCRD_RPC_CERT}
wallethost = ${ALL_WALLET_HOST}
walletuser = ${ALL_WALLET_USER}
walletpass = ${ALL_WALLET_PASS}
walletcert = ${ALL_WALLET_CERT}
loglevel = debug
network = testnet
webserverdebug = false
supportemail = example@test.com
backupinterval = 3m0s
vspclosed = false
vspclosedmsg = Your tickets are no longer welcome here. Please go away.
adminpass=12345
designation = harness
EOF

tmux new-window -t $TMUX_SESSION -n "vspd"

echo "Creating vspd database"
tmux send-keys "go run ./cmd/vspadmin --homedir=${HARNESS_ROOT}/vspd --network=testnet createdatabase ${VSPD_FEE_XPUB}" C-m 
sleep 3 # wait for database creation and ensure dcrwallet rpc listeners are started
echo "Starting vspd"
tmux send-keys "vspd --homedir=${HARNESS_ROOT}/vspd" C-m 

#################################################
# All done - attach to tmux session.
#################################################

tmux attach-session -t $TMUX_SESSION
