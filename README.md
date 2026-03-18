# gctl

Command-line tool for managing [Geeper](https://geeper.io) clusters and joining worker nodes.

## Usage

### Join a worker node to a cluster

```sh
sudo gctl cluster join <join-token>
```

Obtain a join token from the [Geeper console](https://console.geeper.io) under your cluster's **Nodes** tab.

The command will:
1. Download the correct version of k0s for your architecture
2. Write cluster certificates
3. Install and start the k0s worker service

## Building from source

```sh
git clone https://github.com/geeper-io/gctl.git
cd gctl
go build -o gctl .
```

Requires Go 1.23+.
