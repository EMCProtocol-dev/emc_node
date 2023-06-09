# EMC Node
Framework for p2p AI network.

Build your node and join the EMC network immediately
![](https://www.edgematrix.pro/requester/static/images/4c67f2b1e2.png)

## Build
Execute the following command to compile the EMC node for linux_x64, windows_x64, mac(intel), mac_arm64(m1/m2).

```shell
cd ./emc_node/edge-matrix
sh build.sh
```

## Initial node
Execute the following command to init a EMC node.

```shell
cd ../dist/{linux|mac|mac_arm64}/emc
```
```shell
./edge-matrix secrets init --data-dir edge_data 
```
## Run
Execute the following command to run a EMC node.
Command with "--miner-canister nk6pr-3qaaa-aaaam-abnrq-cai" to works with the Testnet miner canister.
```shell
./edge-matrix server --chain genesis.json --data-dir edge_data  --grpc-address 0.0.0.0:50000 --libp2p 0.0.0.0:50001 --jsonrpc 0.0.0.0:50002 --miner-canister nk6pr-3qaaa-aaaam-abnrq-cai 
```

## Basic Usage
Execute the following command to get help.
```shell
./edge-matrix help
```

## Java_sdk
GitHub repository: https://github.com/EMCProtocol-dev/emc_java_sdk

## Js-monorepo sdk
GitHub repository: https://github.com/EMCProtocol-dev/edgematrixjs-monorepo

## Golang sdk
GitHub repository: https://github.com/EMCProtocol-dev/emc_go_sdk

##  AI Sample for Stable Diffusion nodes
Address: https://6tq33-2iaaa-aaaap-qbhpa-cai.icp0.io/

GitHub repository: https://github.com/EMCProtocol-dev/EMC-SD

## Computing Node Test Tools
Address: https://57hlm-riaaa-aaaap-qbhfa-cai.icp0.io

GitHub repository: https://github.com/EMCProtocol-dev/EMC-Requester

## Tutorials
For tutorials, check https://edgematrix.pro/start

License
------
Apache 2.0
