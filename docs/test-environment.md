# 테스트 환경

gantry를 실제 docker / containerd 데몬에 대해 검증하기 위한 devcontainer 기반
테스트 환경 설명. 자동 테스트(`go test`)와 전체 루프 수동 검증, 그리고 그 과정의
네트워크/insecure 제약을 다룬다.

## 토폴로지

devcontainer는 두 서비스로 구성된다 ([.devcontainer/docker-compose.yaml](../.devcontainer/docker-compose.yaml)).

```
┌───────────────────────────────────┐        ┌─────────────────────────────────────┐
│ dev (uid 1000)                    │        │ docker  (image: docker:dind)        │
│  - gantry / go test 실행          │        │  - dockerd  (tcp://0.0.0.0:2375)    │
│  - DOCKER_HOST=tcp://docker:2375 ─┼──────▶│  - 번들 containerd                  │
│                                   │        │      /run/docker/containerd/...sock │
│  /run/docker/containerd/sock ◀━━━┼━shared━┤      (같은 named volume)            │
│  (named volume: containerd.run)   │ volume │                                     │
│                                   │        │  [수동 e2e 시] registry:2 → :5000  │
│  127.0.0.1:5000 ──(local fwd)─────┼──────▶│  127.0.0.1:5000 (dind localhost)    │
└───────────────────────────────────┘        └─────────────────────────────────────┘
```

- **`dev`** — 개발/테스트 컨테이너. gantry와 `go test`가 여기서 돈다. uid 1000.
- **`docker`** — DinD(Docker-in-Docker). `dockerd`가 tcp:2375로 떠 있고, 그 dockerd가
  내부적으로 **containerd**를 구동한다(소켓 `/run/docker/containerd/containerd.sock`).

### 데몬 엔드포인트 두 개

| 종류       | dev에서의 주소                                                  | 비고                                        |
| ---------- | --------------------------------------------------------------- | ------------------------------------------- |
| docker     | `tcp://docker:2375` (`DOCKER_HOST`)                             | 호스트명 `docker`가 dind 컨테이너로 resolve |
| containerd | `/run/docker/containerd/containerd.sock` (`CONTAINERD_ADDRESS`) | 아래 §소켓 공유로 노출                      |

이 둘은 gantry가 데몬에게 "pull해라"라고 **명령**하는 제어 채널일 뿐이다 — 어떤 주소든
무방하며 아래 §insecure 제약과는 무관하다.

## containerd 소켓 공유

dind의 번들 containerd 소켓은 본래 dind 컨테이너 안에만 있어 dev에서 안 보인다.
compose에서 다음으로 노출한다:

1. **공유 볼륨** `containerd.run`을 `docker`·`dev` 양쪽에 `/run/docker/containerd`로
   마운트. (유닉스 소켓은 공유 볼륨의 같은 inode로 커널이 cross-container 연결을
   라우팅하므로 동작한다.)
2. **권한** — 소켓은 `root:root 0660`, 부모 디렉터리는 `0700`이라 dev의 uid 1000이
   그대로는 못 쓴다. dev에서 chmod도 불가. 그래서 `docker` 서비스의 `command`에
   백그라운드 루프를 넣어 dind 쪽에서 `chmod 0711` 디렉터리 + `0666` 소켓으로 열어둔다
   (`exec dockerd`는 PID 1 유지, containerd가 소켓을 재생성해도 3초마다 복구).
3. `dev`에 `CONTAINERD_ADDRESS=/run/docker/containerd/containerd.sock` 환경변수.

> 적용은 **devcontainer rebuild** 시점. 확인:
> ```sh
> ls -l /run/docker/containerd/containerd.sock   # srw-rw-rw- 여야 함
> ```

번들 containerd의 이미지 네임스페이스는 **`moby`** 다(k3s의 `k8s.io`가 아님). target
설정 시 `namespace: "moby"`.

## 자동 테스트 (`go test`)

```sh
go test -race ./...
```

- **단위 테스트**(데몬 불필요): config round-trip, rewrite 규칙, Store 동시성,
  Source copy/proxy(in-memory registry), Warmer, Distributor(fake target) 등.
- **라이브 통합 테스트**(자기-skip): 실데몬이 있으면 돌고 없으면 skip.
  - `internal/down/docker_integration_test.go` → `tcp://docker:2375`에 Ping 후
    `hello-world` 실제 pull.
  - `internal/down/containerd_integration_test.go` → `CONTAINERD_ADDRESS` 소켓이
    있으면 `moby` 네임스페이스로 실제 pull, 없으면 즉시 skip.

## 전체 루프 수동 검증

gantry warm → cache copy → 양쪽 데몬이 cache에서 pull 까지 한 번에 확인한다.

### 1. cache registry + 로컬 포워드

cache registry는 dind 데몬으로 띄워 dind 호스트 `:5000`에 publish한다. dev와 데몬이
**동일한 ref `127.0.0.1:5000/...`** 를 쓰도록 dev에서 `127.0.0.1:5000 → docker:5000`
TCP 포워드를 둔다(이유는 §insecure 제약).

```sh
docker run -d -p 5000:5000 --name cache registry:2

# 의존성 없는 최소 포워더 (scratchpad 등에 두고 백그라운드 실행)
cat > /tmp/fwd.go <<'EOF'
package main
import ("io";"net")
func main(){ l,_:=net.Listen("tcp","127.0.0.1:5000"); for{ c,e:=l.Accept(); if e!=nil{continue};
  go func(c net.Conn){defer c.Close(); u,e:=net.Dial("tcp","docker:5000"); if e!=nil{return};
  defer u.Close(); go io.Copy(u,c); io.Copy(c,u)}(c) } }
EOF
go run /tmp/fwd.go &
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:5000/v2/   # 200
```

### 2. gantry 설정

```yaml
serve:
  addr: "127.0.0.1:18080"
  allow_unknown_stores: true   # let `from: docker.io` resolve to a bare host
  stores:
    - { name: "cache", kind: "registry", host: "127.0.0.1:5000", insecure: true, mode: "copy" }
    - { name: "dind-docker", kind: "docker",     address: "tcp://docker:2375" }
    - { name: "dind-ctr",    kind: "containerd",  address: "/run/docker/containerd/containerd.sock", namespace: "moby" }
  warm:
    platforms: ["linux/amd64"]
```

### 3. job + 확인

```sh
go run . --config gantry-e2e.yaml serve &

curl -s http://127.0.0.1:18080/v1/store         # 3 stores, capabilities, ready

# 사전에 어디에도 없는 이미지로(content-store 캐시 혼동 방지)
ID=$(curl -s -X POST http://127.0.0.1:18080/v1/job \
       -d '{"ref":"busybox:latest","from":"docker.io","to":"cache","distribute":["dind-docker","dind-ctr"]}' \
     | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

curl -s http://127.0.0.1:18080/v1/job/$ID       # transfers[]: cache + each engine
```

기대 결과: `state=done`, cache transfer는 `bytes_done==bytes_total`, `dind-docker`·`dind-ctr` 모두
`pulled`. registry(`curl http://127.0.0.1:5000/v2/library/busybox/tags/list`)와
`docker images`에 적재 확인.

### 4. 정리

```sh
kill %1 %2 2>/dev/null
docker rm -f cache
docker rmi 127.0.0.1:5000/library/busybox:latest busybox:latest 2>/dev/null
```

## insecure 제약 (loopback 한정)

cache가 plain-HTTP면, **다운스트림 데몬이 cache에서 pull할 때** 그 registry를 insecure로
신뢰해야 한다. 이 제약은 **cache registry 주소**에 걸리는 것이지, gantry↔데몬 제어
소켓과는 무관하다.

- **docker 데몬**: `127.0.0.0/8`·`::1`을 자동 insecure 신뢰 → `127.0.0.1:5000` OK.
  비-loopback(`registry.cache.local:5000` 등)은 `daemon.json`의 `insecure-registries` 필요.
- **containerd**: 기본 resolver도 loopback을 plain-HTTP로 자동 처리 → `127.0.0.1:5000` OK.
  비-loopback은 `/etc/containerd/certs.d/<host>/hosts.toml` 필요.

이 테스트가 `127.0.0.1:5000`을 쓰고 포워드까지 두는 이유가 이것이다 — DinD에서 데몬이
**별도 설정 없이** insecure cache를 pull할 수 있는 유일한 주소가 자기 loopback이기 때문.
gantry는 다운스트림의 insecure 정책을 강제하지 않는다(split-brain — gantry 자신의 warm은
`registry.insecure`를 따르지만, 데몬의 cache-pull은 데몬 설정을 따른다). 실 fleet에서
비-loopback insecure cache를 쓰려면 각 데몬에 위 설정을 해두거나 cache에 TLS를 붙여야 한다.

### downstream 호스트 오버라이드

gantry가 **푸시한 주소**와 데몬이 **pull하는 주소**를 분리할 수 있다. `registry.host`로
cache의 실제 위치에 push하되, 데몬에게는 자기가 신뢰하도록 설정해 둔 다른 이름으로 pull하게
시키는 것:

```yaml
registry:
  host: "192.168.0.22:5000"     # gantry가 push/read 하는 실제 주소
  downstream_host: "cache.cr.com"  # 데몬이 pull 하는 주소(전역 기본)
targets:
  - { name: "k3s", kind: "containerd", address: "...", pull_host: "cache.cr.com:5000" }  # per-target override
```

이러면 gantry는 `192.168.0.22:5000/library/redis:7`로 push하지만 데몬에는
`cache.cr.com/library/redis:7`을 pull하라고 시킨다(repo·tag/digest는 그대로, 호스트만 치환).
데몬 쪽에서 `cache.cr.com`을 신뢰(TLS 또는 insecure-registries)하고 DNS/hosts로
`192.168.0.22`를 가리키게 해두면, 비-loopback에서도 insecure 신뢰 문제를 우회할 수 있다.
각 target의 pull ref는 `GET /v1/job/{id}`의 `targets[].ref`로 확인된다.

> dev의 `127.0.0.1:5000`은 포워드를 거쳐 registry로, 데몬의 `127.0.0.1:5000`은 자기
> publish 포트로 — 서로 다른 net namespace지만 **같은 registry**를 가리키고 양쪽 다
> loopback이라 insecure-OK다. 그래서 동일 ref가 양쪽에서 성립한다.
