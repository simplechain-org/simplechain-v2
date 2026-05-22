## Docker Image

Included in this repo is a Dockerfile that you can launch SPC node for trying it out. Docker images are available on `ghcr.io/simplechain-org/simplechain-v2`.

You can build the docker image with the following commands:
```bash
make docker
```

If your build machine has an ARM-based chip, like Apple silicon (M1), the image is built for `linux/arm64` by default. To build for `x86_64`, apply the --platform arg:

```bash
docker build --platform linux/amd64 -t simplechain-org/simplechain-v2 -f Dockerfile .
```

Before start the docker, get a copy of the config.toml & genesis.json from the release: https://github.com/simplechain-org/simplechain-v2/releases, and make necessary modification. `config.toml` & `genesis.json` should be mounted into `/spc/config` inside the container. Assume `config.toml` & `genesis.json` are under `./config` in your current working directory, you can start your docker container with the following command:
```bash
docker run -v $(pwd)/config:/spc/config --rm --name spc -it simplechain-org/simplechain-v2 
```

You can also use `ETHEREUM OPTIONS` to overwrite settings in the configuration file
```bash
docker run -v $(pwd)/config:/spc/config --rm --name spc -it simplechain-org/simplechain-v2 --http.addr 0.0.0.0 --http.port 8545 --http.vhosts '*' --verbosity 3
```

If you need to open another shell, just do:
```bash
docker exec -it simplechain-org/simplechain-v2 /bin/bash
```

We also provide a `docker-compose` file for local testing

To use the container in kubernetes, you can use a configmap or secret to mount the `config.toml` & `genesis.json` into the container
```bash
containers:
  - name: spc
    image: simplechain-org/simplechain-v2

    ports:
      - name: p2p
        containerPort: 30311  
      - name: rpc
        containerPort: 8545
      - name: ws
        containerPort: 8546     

    volumeMounts:
      - name: spc-config
        mountPath: /spc/config

  volumes:
    - name: spc-config
      configMap:
        name: cm-spc-config
```

Your configmap `spc-config` should look like this:
```
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-spc-config
data:
  config.toml: |
    ...

  genesis.json: |
    ...  

```