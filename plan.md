로컬 cache container registry를 heat 시키기위해 pull 요청을 하는 worker를 띄워놓고, 어떤 이미지를 얼마나 받았는지 확인할 수 있는 API를 제공하는 서버를 구현하고싶어.
서버는 또, pull을 다 받고 나면 docker, containerd 등 downstream에 cache registry로부터 이미지 pull 시키는 것도 트리거 시킬 수 있어.
downstream은 seam으로 표현되야겠지. docker daemon 엔드포인트 socket, containerd의 엔드포인트, socket 목록을 config으로 받아서 각각 유형에 맞는 구현이 필요하겠지.
일단 1차적으로 이까지만 기능 구현을 할거고, 그 다음에 이미지 sign 검증이나 설정으로 등록된 각 docker나 containerd에 이미지 GC 기능까지 구현할 생각이야.

일단 HTTP API 엔드포인트 설계, 서버 구조를 어떻게 설계하고 어떻게 구현할건지 계획은 세워줘

---

# gantry phase-1 설계

## 개요

`gantry serve`라는 long-lived subcommand 하나가 in-process **warm engine**과 stdlib `net/http` API 서버를 함께 띄운다. 핵심은 **seam-first** 구조다. 파이프라인 전체가 하나의 동사 `Pull(ctx, ref, sink)`로 추상화되고, 이 동사를 세 군데에 같은 모양으로 박는다 — `Source`(cache registry를 향한 puller), `Target`의 fan(downstream docker/containerd daemon들), 그리고 둘을 묶는 `Warmer`. warm worker는 multi-arch ref의 모든 layer를 cache로 들여보낸다 — 기본 `copy` 모드는 upstream에서 pull 해 cache로 push(이미 있는 blob은 HEAD로 skip), 대안 `proxy` 모드는 cache를 통해 read→discard 해서 pull-through cache(zot / distribution proxy)가 self-fill 하게 유발. 어느 쪽이든 cache로 실제 이동한 byte 진행률을 atomic counter로 노출한다. warm이 끝나면 같은 ref를 등록된 각 `Target`으로 fan-out 해서 downstream daemon들이 이제 hot 해진 cache로부터 pull 하게 트리거한다. 다운스트림으로 갈수록 갈아끼울 수 있는 부분은 전부 `internal/down`(Target + capability sub-interface)과 `internal/warm`(Source/Warmer/Store) 뒤에 격리한다. 새 downstream KIND = `internal/down`에 파일 하나 + factory에 case 하나. phase-2의 verify/GC = Target이 optional capability interface를 추가로 만족시키기만 하면 됨 — phase-1 코드는 한 줄도 안 바뀐다.

데이터 흐름 한 줄: **warm**(`POST /v1/job` → worker가 copy=upstream→cache push / proxy=cache read-through, 이동 byte 카운트) → **status**(`GET /v1/job/{id}` 가 atomic 진행률을 lock-free로 반환, `/progress`로 SSE 스트림) → **distribute**(warm 완료 시 각 `Target.Pull(ctx, ref)` fan-out → docker/containerd가 cache에서 pull).

## 도메인 모델

```go
// internal/warm/job.go
type Job struct {
	ID         string   // ULID
	Ref        string   // canonical upstream ref, e.g. "docker.io/library/redis:7"
	CacheRef   string   // rewrite(Ref) 결과. copy=push 위치 / proxy=pull 위치 / downstream trigger
	Platforms  []string // 선택된 플랫폼들. 비면 config 기본값
	State      JobState // pending -> pulling -> warm -> triggering -> done | failed | canceled
	BytesTotal int64    // manifest descriptor 크기 합. 첫 byte 이동 전에 확정
	BytesDone  *atomic.Int64
	Layers     []*LayerProgress
	Targets    []*TargetProgress
	Err        string
	CreatedAt  time.Time
	StartedAt  time.Time
	EndedAt    time.Time
	cancel     context.CancelFunc // unexported; DELETE가 호출
}

type JobState string

const (
	JobPending    JobState = "pending"
	JobPulling    JobState = "pulling"
	JobWarm       JobState = "warm"
	JobTriggering JobState = "triggering"
	JobDone       JobState = "done"
	JobFailed     JobState = "failed"
	JobCanceled   JobState = "canceled"
)

// 한 blob(layer 또는 config)의 byte 진행률.
type LayerProgress struct {
	Digest   string // sha256:...
	Platform string // "linux/amd64"
	Total    int64  // descriptor.Size (compressed, on-wire)
	Done     *atomic.Int64
	State    string // pending | pulling | warm | exists | failed  (exists=copy 모드에서 이미 cache에 있어 skip)
}

// 한 downstream target으로의 pull 결과.
type TargetProgress struct {
	Name  string
	Kind  string // "docker" | "containerd"
	State string // pending | pulling | pulled | failed
	Err   string
}

// internal/warm/warm.go
// byte가 움직이기 전에 Source.Resolve가 만드는 manifest plan. BytesTotal과 fan-out할 layer 목록을 준다.
type Plan struct {
	Ref    string
	Layers []PlannedLayer
	Total  int64
}

type PlannedLayer struct {
	Digest   string
	Repo     string // copy 모드: cache로 매핑할 repo path
	Platform string
	Size     int64
}

// internal/down/down.go
// down.Capabilities(t)가 만드는 capability descriptor. GET /v1/target에 직렬화되어
// Target이 optional interface를 구현하는 순간 phase-2 기능이 관측 가능해진다.
type Caps struct {
	Pull   bool
	Verify bool
	GC     bool
}
```

## 패키지 구조

기존 컨벤션(`cmd/` 한 파일 = 한 subcommand, `cmd/config/` 한 파일 = 한 sub-config)을 그대로 따른다. 엔진 로직은 `internal/`로 분리하는데, 이유는 (1) warm/down은 cmd 패키지에 두기엔 너무 무겁고 테스트가 독립적이어야 하며, (2) seam(Target/Source)이 `cmd`에 의존하지 않아야 갈아끼우기·재사용이 가능하기 때문. 패키지 경계가 곧 seam 경계다.

```
cmd/
  serve.go                  NewCmdServe() *xli.Command. root에 wire. Source/Target/Warmer/http.Server 조립 후 serve + graceful shutdown.
cmd/config/
  config.go                 (기존) Config에 Serve ServeConfig 필드 추가, Evaluate에 기본값.
  serve.go                  ServeConfig + AuthConfig + RegistryConfig + WarmConfig + TargetConfig. greet.go 패턴.
  duration.go               named type Duration(= time.Duration) + encoding.TextMarshaler/Unmarshaler. (프로젝트 컨벤션: bare time.Duration 금지)
  rewrite.go                RewriteRule(= {pattern: template}) + BytesUnmarshaler + compile(). source ref → cache ref 매핑 규칙.
internal/warm/
  warm.go                   Source interface + Warmer. Warmer.Run이 Source 구동 후 Target들로 fan-out. countingReader, ProgressSink 정의.
  source_copy.go            copySource(기본): warm.Source를 upstream pull→cache push(go-containerregistry remote)로 구현. blob HEAD skip + per-blob progress.
  source_proxy.go           proxySource: cache read→discard로 pull-through self-fill 유발. ggcr를 import하는 건 이 둘뿐(레지스트리 라이브러리 교체 시 여기만 수정). mode가 둘을 고름.
  rewrite.go                Rewrite(rules, cacheHost, ref) → cache ref. doublestar 매칭 + text/template 렌더 + identifier append. copy/proxy/trigger 공용.
  job.go                    Job/LayerProgress/TargetProgress/Plan/PlannedLayer 도메인 타입 + Store interface + in-memory memStore.
  store.go                  memStore: map[ID]*Job + sync.RWMutex. 완료 job TTL eviction. Store가 interface이므로 phase-2에 bbolt/sqlite 무중단 교체.
internal/down/
  down.go                   다운스트림 seam — 설계의 중심. Target interface + capability sub-interface(Verifier, Collector). Registry(name->Target). Capabilities() 헬퍼. 3rd-party import 없음(seam은 의존성-free).
  factory.go                NewTarget(TargetConfig) (Target, error) — Kind 위의 단일 switch. 새 KIND = case 하나 + 파일 하나.
  docker.go                 dockerTarget: github.com/docker/docker/client. 한 파일 = 한 KIND.
  containerd.go             containerdTarget: github.com/containerd/containerd/v2/client. k8s.io namespace.
internal/server/
  server.go                 New(deps) *http.Server. Go 1.22+ ServeMux method+path 패턴 조립. *warm.Warmer, warm.Store, down.Registry 보유. 순수 transport(decode->core->encode).
  handlers.go               handleCreateWarm/handleGetWarm/handleListWarms/handleListTargets/handleTriggerPull/handleProgress. handleListTargets가 down.Capabilities(t) 호출 — phase-2가 여기서 자동 노출.
  auth.go                   Auth(AuthConfig) 미들웨어. /v1/* = bearer token 화이트리스트 OR 검증된 mTLS client cert. /healthz bypass. token 비교는 constant-time(subtle.ConstantTimeCompare).
```

## HTTP API

`/v1` prefix + stdlib `ServeMux` method+path 패턴(`"POST /v1/job"`, `"GET /v1/job/{id}"`). **`/v1/*`는 인증 미들웨어 뒤에 둔다**(아래 §인증) — bearer token + optional mTLS. `/healthz`만 인증 면제(Nomad/k3s probe).

| Method | Path | 목적 |
|---|---|---|
| POST | `/v1/job` | warm job 생성·enqueue. (ref, 정규화된 platform set)으로 idempotent: active job이 있으면 200으로 그 job 반환(dedup). |
| GET | `/v1/job` | job 목록. `?state=` `?ref=`(substring) 필터. "어떤 이미지를 얼마나" 질의. |
| GET | `/v1/job/{id}` | 한 job의 전체 상태: per-layer / per-target 분해 진행률. |
| GET | `/v1/job/{id}/progress` | 진행률 delta의 SSE 스트림(stdlib `http.Flusher`, 새 의존성 없음). `?wait=`로 long-poll fallback. |
| DELETE | `/v1/job/{id}` | in-flight job 취소(context cancel) 또는 완료된 job record evict. |
| GET | `/v1/target` | 등록된 downstream target과 각자의 capability 목록. phase-2 seam을 관측 가능하게 만드는 엔드포인트. |
| POST | `/v1/target/{name}/pull` | warm job 없이 downstream pull만 트리거(cache가 이미 hot이거나 수동 reconcile). Target seam을 Warmer와 분리. |
| GET | `/healthz` | Nomad/k3s liveness. config/registry 의존 없음. |

**POST /v1/job** (요청)
```json
{
  "ref": "docker.io/library/redis:7",
  "platforms": ["linux/amd64", "linux/arm64"],
  "targets": ["nomad-docker", "k3s"],
  "trigger_downstream": true
}
```
`ref`는 mode와 무관하게 **항상 canonical upstream ref**(예 `docker.io/library/redis:7`)다. gantry가 `registry.rewrite` 규칙으로 cache-side ref를 산출 → copy면 그 위치로 push, proxy면 그 위치에서 pull, downstream 트리거도 같은 cache ref. 즉 caller는 cache 토폴로지를 몰라도 됨(rewrite가 흡수). 응답의 `ref`는 입력 그대로, 별도로 `cache_ref`(매핑 결과)를 함께 반환.
`platforms`는 **요청에서 지정하는 것이 primary**(caller가 어떤 arch를 데울지 결정) — 생략 시에만 `WarmConfig.Platforms`(설정 fallback), 그것도 없으면 gantry 호스트 `runtime.GOOS/GOARCH`. `targets`/`trigger_downstream`은 optional(targets 생략 시 전체, trigger 기본 true).

응답 `201`(dedup 시 `200`), `Location: /v1/job/wrm_01J...`
```json
{
  "id": "wrm_01J...",
  "ref": "docker.io/library/redis:7",
  "cache_ref": "registry.cache.local/library/redis:7",
  "state": "pulling",
  "bytes_total": 410582931,
  "bytes_done": 0,
  "platforms": ["linux/amd64", "linux/arm64"],
  "created_at": "2026-06-30T16:50:00Z"
}
```

**GET /v1/job/{id}** (응답)
```json
{
  "id": "wrm_01J...",
  "ref": "docker.io/library/redis:7",
  "cache_ref": "registry.cache.local/library/redis:7",
  "state": "triggering",
  "bytes_total": 410582931,
  "bytes_done": 410582931,
  "layers": [
    {"digest": "sha256:ab..", "platform": "linux/amd64", "total": 28311552, "done": 28311552, "state": "warm"},
    {"digest": "sha256:cd..", "platform": "linux/arm64", "total": 27983616, "done": 19922944, "state": "pulling"}
  ],
  "targets": [
    {"name": "nomad-docker", "kind": "docker", "state": "pulled", "error": ""},
    {"name": "k3s", "kind": "containerd", "state": "pulling", "error": ""}
  ],
  "error": ""
}
```

**GET /v1/job** (응답) — 각 항목이 bytes_total/bytes_done를 들고 있어 job을 안 열어도 집계가 보임.
```json
{"items": [
  {"id": "wrm_01J...", "ref": "...", "state": "pulling", "bytes_total": 410582931, "bytes_done": 256901120, "layers": 7, "layers_done": 4, "created_at": "...", "updated_at": "..."}
]}
```

**GET /v1/target** (응답) — capability가 곧 phase-2 self-description.
```json
{"items": [
  {"name": "nomad-docker", "kind": "docker", "address": "/var/run/docker.sock", "ready": true,
   "capabilities": {"pull": true, "verify": false, "gc": false}},
  {"name": "k3s", "kind": "containerd", "address": "/run/k3s/containerd/containerd.sock", "namespace": "k8s.io", "ready": true,
   "capabilities": {"pull": true, "verify": false, "gc": false}}
]}
```

**GET /v1/job/{id}/progress** (SSE)
```
event: progress
data: {"bytes_done": 256901120, "bytes_total": 410582931, "layers": [{"digest":"sha256:cd..","done":19922944}]}

event: state
data: {"state": "triggering"}

event: done
data: {"state": "done"}
```
주의: downstream UI는 Traefik / kamino reverse-proxy 뒤에 있다(project memory). SSE를 쓰려면 그 hop에서 proxy buffering을 꺼야 한다. 끄기 곤란하면 `?wait=` long-poll로 폴백.

**GET /healthz** → `200` body `ok`.

## 캐시 워밍 메커니즘

라이브러리: **github.com/google/go-containerregistry**(ggcr) — `pkg/v1/remote`(pull/push), `pkg/name`(ref), `pkg/v1`(Image/Index/Layer), `pkg/authn`(creds), `pkg/v1/types`(mediatype 분기), `pkg/crane`(copy helper). 순수 Go라 `CGO_ENABLED=0` scratch 빌드 유지. (모듈 경로 `/v2` suffix 없음 — `github.com/google/go-containerregistry`, 현재 태그 v0.x.)

워밍은 `warm.Source` interface 뒤의 **두 구현**으로 나뉘고 `registry.mode`로 고른다. 기본은 **`copy`**(gantry가 upstream→cache로 직접 옮김 — "얼마나 옮겼는지"를 정직하게 추적, cache writable 필요), 대안은 **`proxy`**(pull-through cache가 self-fill 하도록 유발, cache read-only proxy용).

```go
// internal/warm/warm.go
type Source interface {
	// ref를 manifest까지 풀어 Plan(layer 목록 + BytesTotal)을 만든다(byte 이동 전).
	Resolve(ctx context.Context, ref string, platforms []string) (*Plan, error)
	// 한 blob을 cache로 들여보낸다(copy=push, proxy=read-through). sink로 진행률 보고.
	Warm(ctx context.Context, l PlannedLayer, sink ProgressSink) error
}
```

**copy 모드** (`source_copy.go`, 기본). gantry가 **upstream에서 pull → cache로 push**. cache는 writable registry여야 한다.
1. 요청 ref(=upstream/canonical, 예 `docker.io/library/redis:7`)를 `name.ParseReference`로 연다. dst ref = `warm.Rewrite(registry.rewrite, registry.host, ref)`(§설정 rewrite 규칙). 같은 dst ref가 downstream 트리거에도 쓰임 → push 위치와 daemon pull 위치가 항상 일치.
2. `remote.Get` → index면 `.ImageIndex()` 아니면 `.Image()`. 선택 platform manifest마다 `Layers()` + config blob 열거.
3. blob마다 **먼저 cache에 존재하는지 HEAD**(`remote.Head`/`dstLayer.Exists()`) → 있으면 `state="exists"`로 skip(전송 0, 증분만 집계). 없으면 progress-wrapped `srcLayer`를 `remote.WriteLayer(dstRepo, layer)`로 push.
4. blob이 다 올라가면 manifest(+index)를 `remote.Write`/`remote.Put`으로 push(의존 순서: blob → manifest → index). `crane.Copy(src, dst, remote.WithProgress(ch))`로 한 번에 갈 수도 있다(존재검사·skip을 ggcr이 내부 처리) — per-blob 정밀 진행률이 필요하면 위처럼 layer 단위로 직접. **진행률 = cache로 실제 전송된 byte**(skip된 blob은 0)라 "얼마나 받았는지"가 정직.

**proxy 모드** (`source_proxy.go`). cache가 read-only pull-through proxy일 때. gantry가 **cache에서 blob을 EOF까지 read→discard** → miss면 cache가 upstream self-fill. **각 blob을 끝까지 완전히 읽어야** cache가 commit(HEAD/부분 읽기는 cold — zot은 모든 blob이 `.sync`에 다 내려와야 image promote).
1. pull 소스 = `warm.Rewrite(registry.rewrite, registry.host, ref)` → **cache host ref**(proxy가 upstream 매핑). 2. `remote.Get`(cache) → index/image 분기, layer 열거. 3. blob마다 `layer.Compressed()`(`Uncompressed()` 아님 — on-wire byte)를 `io.Copy(io.Discard, countingReader)`로 전부 read. 전체 read가 cache의 upstream fetch·persist 트리거. (이 모드는 증분 skip 불가 — 전체 read가 목적.)

진행률: `Plan`을 먼저 만들어 모든 layer+config의 `descriptor.Size` 합을 `BytesTotal`로 확정(byte 이동 전에 분모 확정). 복사 중엔 각 layer를 `countingReader`로 감싸 per-layer atomic과 per-job atomic을 매 `Read`마다 증가. status API는 이 atomic을 직접 읽으므로 in-flight job에서 실시간 진행률이 나오고 copy loop와 lock 경합이 없다. 로버스트니스 보강: per-layer wrapping을 건너뛴 경우를 위한 cross-check로 `remote.WithProgress(chan v1.Update)`를 whole-pull byte/complete fallback으로 함께 단다.

multi-arch: ref가 `v1.ImageIndex`(OCI index / docker manifest list)로 resolve될 수 있다. 데울 arch는 **요청의 `platforms`가 primary**(예: `["linux/amd64","linux/arm64"]`), 생략 시 `WarmConfig.Platforms`(설정 fallback), 그것도 없으면 gantry 호스트 `runtime.GOOS/GOARCH`만. `index.IndexManifest().Manifests`를 `descriptor.Platform`으로 필터하되 **`Platform==nil`이나 `os=="unknown"`(attestation/SBOM 항목)은 skip**(이미지 manifest가 아님). 혼합 x86_64/aarch64 fleet을 위해 선택된 **모든** platform을 데우고, downstream 트리거는 **index digest**로 보내서 각 daemon이 자기 arch를 hot cache에서 고르게 한다.

```go
// internal/warm/source_copy.go (기본 모드 sketch) — upstream blob을 cache로 push.
func (s *copySource) Warm(ctx context.Context, l PlannedLayer, sink ProgressSink) error {
	dst, err := name.NewDigest(s.cacheRef(l.Repo, l.Digest), s.nameOpts...) // cache host로 매핑
	if err != nil {
		return z.Err(err, "dst ref %q", l.Digest)
	}
	if ok, _ := s.exists(ctx, dst); ok { // HEAD: 이미 있으면 전송 0
		sink.SetState("exists")
		return nil
	}
	src, err := remote.Layer(s.srcDigest(l), s.pullOpts(ctx)...) // upstream에서 pull
	if err != nil {
		return z.Err(err, "resolve src layer")
	}
	layer := &countingLayer{Layer: src, sink: sink}        // Compressed() Read마다 atomic 증가
	if err := remote.WriteLayer(dst.Repository, layer, s.pushOpts(ctx)...); err != nil {
		return z.Err(err, "push blob")
	}
	sink.SetState("warm")
	return nil
}
// proxy 모드(source_proxy.go)는 위 §proxy 모드대로 layer.Compressed()를 io.Copy(io.Discard, countingReader)로 EOF까지 read.
```

## 다운스트림 seam

```go
// internal/down/down.go — 다운스트림 seam.
// Target은 모든 downstream KIND가 반드시 만족해야 하는 phase-1 최소 계약.
type Target interface {
	Name() string
	Kind() string // "docker" | "containerd"
	Ready(ctx context.Context) error // /v1/target 도달성 체크
	// Pull은 downstream daemon이 cache registry에서 ref를 pull 하게 만든다.
	Pull(ctx context.Context, ref string, sink ProgressSink) error
	Close() error
}

// --- phase-2 capability sub-interface. Target이 OPTIONAL하게 구현한다.
// 서버는 type assertion으로 발견하고, 추가는 그 target 파일 + factory만 건드린다.
type Verifier interface { // cosign/sigstore 서명 검증
	Verify(ctx context.Context, ref string, policy VerifyPolicy) (VerifyResult, error)
}
type Collector interface { // downstream daemon에서 image GC
	Collect(ctx context.Context, opts GCOptions) (GCReport, error)
}

// 어떤 optional interface를 구현하는지 보고 — GET /v1/target를 구동하고
// phase-2 기능이 handler 수정 없이 light up 하게 한다.
func Capabilities(t Target) Caps {
	_, vfy := t.(Verifier)
	_, gc := t.(Collector)
	return Caps{Pull: true, Verify: vfy, GC: gc}
}

func NewTarget(c config.TargetConfig) (Target, error) { // factory.go
	switch c.Kind {
	case "docker":
		return newDockerTarget(c)
	case "containerd":
		return newContainerdTarget(c)
	default:
		return nil, z.Err(nil, "unknown target kind %q", c.Kind)
	}
}
```

**docker impl** (`internal/down/docker.go`). Engine v28 기준 import `github.com/docker/docker/client`로 `client.NewClientWithOpts(client.WithHost("unix://"+c.Address), client.WithAPIVersionNegotiation())`. `Pull`: `rc, err := cli.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: enc})`(옵션 타입은 `github.com/docker/docker/api/types/image`의 `PullOptions`, 반환은 `(io.ReadCloser, error)`) 후 `github.com/docker/docker/pkg/jsonmessage`의 `JSONMessage`를 line-decode 해서 `.Status`/`.ProgressDetail.Current` delta를 sink로 보내고 **반드시 EOF까지 drain·Close**(안 그러면 pull 미완, 에러는 return이 아니라 stream의 `.Error`에 실린다). `RegistryAuth`는 `registry.AuthConfig`(`github.com/docker/docker/api/types/registry`)를 json marshal → **base64url**(`base64.URLEncoding`, Std 아님). `Ready`: `cli.Ping(ctx)`. 순수 Go → scratch OK.
> 버전 주의: Engine v29+는 모듈이 `github.com/moby/moby/client`로 rename되고 `ImagePull`이 `ImagePullResponse`(io.ReadCloser + `JSONMessages()/Wait()`)를 반환. rename을 잇는 replace-directive는 없음. phase-1은 가장 문서화된 **v28 + github.com/docker/docker/client**로 핀.

**containerd impl** (`internal/down/containerd.go`). import `containerd "github.com/containerd/containerd/v2/client"` + `"github.com/containerd/containerd/v2/pkg/namespaces"`(v2 경로 주의: `/v2/pkg/namespaces`, core 타입은 `/v2/core/*`). `containerd.New(c.Address)`(k3s는 `/run/k3s/containerd/containerd.sock`). `Pull`: `ctx = namespaces.WithNamespace(ctx, c.Namespace /* k3s는 k8s.io */)` 후 `cli.Pull(ctx, ref, containerd.WithPullUnpack)`. multi-arch는 daemon이 index에서 노드 arch를 고름. 진행률은 native byte stream이 없으므로 `cli.ContentStore().ListStatuses`를 goroutine에서 ticker로 polling 해 ingest ref↔layer digest를 상관시킨다(이미 store에 있는 layer는 Status를 안 냄). phase-1에서 byte polling이 과하면 **state-level(pulling/pulled)** 로만 보고 — docker는 byte-level, containerd는 once-at-complete로 기대치를 맞춘다. `Ready`: `cli.Version(ctx)`. 순수 Go(download/unpack은 daemon-side) → scratch CGO=0 OK.

주의(seam 공통): insecure/plain-HTTP은 split-brain이다. gantry 자신의 warm pull은 `remote.WithTransport(insecure)`/`name.Insecure`를 따르지만, downstream의 cache-pull은 **daemon 설정**(docker `daemon.json` insecure-registries, containerd `/etc/containerd/certs.d` hosts.toml)을 따르며 gantry가 강제 못 한다. gantry가 잘 데운 self-signed/HTTP cache라도 downstream pull은 실패할 수 있고, 그 에러는 `Pull()` return이 아니라 docker jsonmessage stream / containerd gRPC 에러 안에 있다 → `TargetProgress.Err`로 명시적으로 surface. (hand-rolled `docker.Resolver`는 daemon의 certs.d를 자동으로 안 읽음 — 필요 시 insecure resolver를 직접 구성.)

**phase-2 성장 경로**: Verify는 `dockerTarget`/`containerdTarget`에 `Verifier`를 구현(또는 `Target`을 embed 하는 decorator)하고 `github.com/sigstore/cosign/v2`로 cache registry against 검증. Warmer에 `if v, ok := t.(down.Verifier); ok { ... }` 한 줄과 `VerifyConfig` 필드만 추가, `Capabilities()`가 이미 `verify:true`를 노출하므로 API는 자동 self-describe. GC는 `Collector`를 구현(docker `ImagesPrune`/`ImageRemove`, containerd images service + content GC)하고 `POST /v1/target/{name}/gc`에서 `c, ok := t.(down.Collector); if !ok { 405 }`. 둘 다 additive: 새 파일/메서드 + 새 config struct + optional 새 endpoint. `Target`/`Source`/`Warmer`/`Store`/기존 handler는 무변경. (가드: capability-by-assertion은 컴파일 타임 보장이 없다 — 새 Target에 인터페이스 구현을 깜빡하면 build 에러 대신 조용히 `verify:false`. startup 로그 한 줄 + KIND별 기대 capability를 단언하는 테스트로 방어.)

## 설정(config)

기존 `Config{Greet, Otel}`의 flat 형태를 유지하기 위해 warm/registry/targets를 하나의 top-level `ServeConfig` 아래로 nest 해서 `Config`에 `Serve` 필드로 붙인다(Greet/Otel과 동일한 nesting). duration 필드는 프로젝트 컨벤션대로 **named type + `encoding.TextMarshaler`**를 쓰고 bare `time.Duration`은 안 쓴다. (주의: `cv.Duration` 같은 타입은 gantry에 존재하지 않는다 — 'cove' 프로젝트 잔재이므로 import 가정 금지. `cmd/config/duration.go`에 직접 정의.)

```yaml
# gantry.yaml — phase-1 추가분 (greet/otel 블록은 그대로)
serve:
  addr: ":8080"
  shutdown_grace: 15s          # cmd/config/duration.go의 named Duration

  # /v1/* 인증. /healthz는 면제.
  auth:
    # bearer token. 하나 이상이면 Authorization: Bearer <token> 필수.
    # 환경변수 확장(예: ${GANTRY_TOKEN})은 Evaluate에서 처리.
    tokens: ["${GANTRY_TOKEN}"]
    # optional mTLS: client cert가 이 CA로 검증되면 통과(token과 OR).
    # client_ca: "/etc/gantry/ca.pem"
    # 서버 TLS(mTLS·HTTPS API용). 비면 평문 HTTP(reverse-proxy가 TLS 종단).
    # tls_cert: "/etc/gantry/server.pem"
    # tls_key:  "/etc/gantry/server-key.pem"

  # cache registry. copy 모드=push 대상, proxy 모드=pull 소스.
  registry:
    mode: copy                 # copy(기본, upstream→cache push) | proxy(cache self-fill)
    host: "registry.cache.local"  # = {{.CacheHost}}
    insecure: true             # plain-HTTP / self-signed (fleet 흔함)
    username: ""               # cache 자격증명 (copy=push / proxy=pull). 비면 익명
    password: ""
    # copy 모드 upstream pull 자격증명은 ~/.docker/config.json(authn.DefaultKeychain) 사용.

    # source ref → cache ref 매핑. 순서대로 평가, first-match-wins.
    # {pattern: template} 단일 키 맵(배열이라 순서 보존). 모든 ref(yaml에선 따옴표 필수).
    rewrite:
      - { "ghcr.io/**": "{{.CacheHost}}/{{.Repo}}" }            # ghcr → cache/<repo>
      - { "**":         "{{.CacheHost}}/{{.Registry}}/{{.Repo}}" }  # 나머지 → host prefix로 충돌 회피
    # 예) "{{.Full}}" = 그대로(identity), "cache/{{.Repo}}" = 리터럴 host 'cache'로.
    # 템플릿에 tag/digest 없으면 source의 identifier 자동 append.

  # warming 동작.
  warm:
    platforms: ["linux/amd64", "linux/arm64"]  # 비면 gantry 호스트 GOOS/GOARCH만
    jobs: 2                    # 동시 warm job 수(worker pool 크기)
    concurrency: 4             # job당 동시 layer pull 상한
    queue_size: 256
    job_ttl: 30m               # 완료 job 보존 기간(Duration)
    trigger_downstream: true

  # downstream daemon들. 각 entry의 kind가 down.Target 구현을 고른다.
  targets:
    - name: "nomad-docker"
      kind: "docker"
      address: "/var/run/docker.sock"
    - name: "k3s"
      kind: "containerd"
      address: "/run/k3s/containerd/containerd.sock"
      namespace: "k8s.io"      # containerd 전용 knob
      platforms: ["linux/arm64"] # per-target override(arm64-only 노드)

  # ---- phase-2 (설계만, 미구현) — seam을 보여주려 여기 둠 ----
  # verify: { enabled: false, policy: "keyless" }
  # gc:     { schedule: "" }
```

기존 패턴 매핑 — `cmd/config/serve.go`:
```go
package config

type ServeConfig struct {
	Addr          string         `yaml:"addr"`
	ShutdownGrace Duration       `yaml:"shutdown_grace"` // bare time.Duration 금지
	Auth          AuthConfig     `yaml:"auth"`
	Registry      RegistryConfig `yaml:"registry"`
	Warm          WarmConfig     `yaml:"warm"`
	Targets       []TargetConfig `yaml:"targets"`
}

type AuthConfig struct {
	Tokens   []string `yaml:"tokens"`    // bearer token 화이트리스트(비면 token 검사 off)
	ClientCA string   `yaml:"client_ca"` // optional mTLS client CA (token과 OR)
	TLSCert  string   `yaml:"tls_cert"`  // 서버 TLS cert (비면 평문 HTTP)
	TLSKey   string   `yaml:"tls_key"`
}

type RegistryConfig struct {
	Mode     string        `yaml:"mode"`     // "copy"(기본) | "proxy"
	Host     string        `yaml:"host"`     // cache host = {{.CacheHost}}. copy=push 대상, proxy=pull 소스
	Insecure bool          `yaml:"insecure"` // cache가 plain-HTTP/self-signed
	Username string        `yaml:"username"` // cache 자격증명. copy 모드 upstream은 DefaultKeychain
	Password string        `yaml:"password"`
	Rewrite  []RewriteRule `yaml:"rewrite"`  // source ref → cache ref 매핑(순서=우선순위)
}

type WarmConfig struct {
	Platforms         []string `yaml:"platforms"`
	Jobs              int      `yaml:"jobs"`
	Concurrency       int      `yaml:"concurrency"`
	QueueSize         int      `yaml:"queue_size"`
	JobTTL            Duration `yaml:"job_ttl"`
	TriggerDownstream bool     `yaml:"trigger_downstream"`
}

type TargetConfig struct {
	Name      string   `yaml:"name"`
	Kind      string   `yaml:"kind"`      // docker | containerd
	Address   string   `yaml:"address"`
	Namespace string   `yaml:"namespace"` // containerd
	Platforms []string `yaml:"platforms"` // per-target override
}
```

`cmd/config/duration.go`:
```go
package config

import "time"

// Duration은 yaml에 "15s"처럼 쓰기 위한 named type. bare time.Duration을 쓰지 않는 컨벤션.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
```

`cmd/config/rewrite.go` — source glob → cache ref 템플릿:
```go
package config

import (
	"fmt"
	"text/template"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/z"
)

// RewriteRule은 {pattern: template} 단일 키 맵으로 기술(배열 순서 = 우선순위).
type RewriteRule struct {
	Pattern  string             // source glob: "**", "ghcr.io/**"
	Template string             // dest ref 템플릿: "{{.Full}}", "{{.CacheHost}}/{{.Repo}}"
	tmpl     *template.Template // Evaluate에서 컴파일
}

// goccy BytesUnmarshaler. {"ghcr.io/**": "..."}를 단일 키로 받는다.
func (r *RewriteRule) UnmarshalYAML(b []byte) error {
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	if len(m) != 1 {
		return fmt.Errorf("rewrite rule must be one {pattern: template}, got %d keys", len(m))
	}
	for k, v := range m {
		r.Pattern, r.Template = k, v
	}
	return nil
}

func (r *RewriteRule) compile() error {
	t, err := template.New(r.Pattern).Option("missingkey=error").Parse(r.Template)
	if err != nil {
		return z.Err(err, "rewrite template %q", r.Template)
	}
	r.tmpl = t
	return nil
}
```

매핑 의미 (`internal/warm/rewrite.go`가 copy push-dest / proxy pull-src / downstream trigger 모두에서 동일 함수로 사용):
- **매칭**: `doublestar.Match(pattern, ref.Name())`. `**`는 `/`를 넘나들고 `*`는 한 segment. 정규화된 full ref에 평가, 첫 매치 사용, 매치 없으면 에러.
- **템플릿 변수**: `.Ref`(원본 문자열) `.Full`(정규화 ref) `.CacheHost`(=registry.host) `.Registry`(예 `index.docker.io`) `.Repo`(예 `library/redis`) `.Tag` `.Digest` `.Identifier`.
- **identifier 보존**: 렌더 결과에 tag/digest가 없으면 source `.Identifier`를 자동 append → `cache/{{.Repo}}`도 `:7` 유지. 명시하려면 `{{.Tag}}`/`{{.Digest}}`.
- **gotcha**: ggcr이 Docker Hub를 `index.docker.io/library/...`로 정규화 → `.Registry`는 `docker.io`가 아닌 `index.docker.io`. 패턴도 감안(`index.docker.io/**` 또는 `**`).
- **dep**: `github.com/bmatcuk/doublestar/v4`(순수 Go, CGO=0). 의존 최소화를 원하면 glob→regexp ~30줄 in-house로 대체 가능.

`cmd/config/config.go`의 `Config`에 `Serve ServeConfig` 추가, `Evaluate()`에 `z.FallbackP`로 기본값:
```go
func (c *Config) Evaluate() error {
	z.FallbackP(&c.Greet.Format, "Hello, %s!")
	z.FallbackP(&c.Serve.Addr, ":8080")
	z.FallbackP((*time.Duration)(&c.Serve.ShutdownGrace), 15*time.Second)
	z.FallbackP(&c.Serve.Registry.Mode, "copy")
	z.FallbackP(&c.Serve.Warm.Jobs, 2)
	z.FallbackP(&c.Serve.Warm.Concurrency, 4)
	z.FallbackP(&c.Serve.Warm.QueueSize, 256)
	z.FallbackP((*time.Duration)(&c.Serve.Warm.JobTTL), 30*time.Minute)
	for i := range c.Serve.Targets {
		if c.Serve.Targets[i].Kind == "containerd" {
			z.FallbackP(&c.Serve.Targets[i].Namespace, "k8s.io")
		}
	}
	// rewrite가 비면 mode별 기본 규칙 1줄, 그다음 전부 compile (fail-fast).
	if len(c.Serve.Registry.Rewrite) == 0 {
		c.Serve.Registry.Rewrite = []RewriteRule{{Pattern: "**", Template: "{{.CacheHost}}/{{.Repo}}"}}
	}
	for i := range c.Serve.Registry.Rewrite {
		if err := c.Serve.Registry.Rewrite[i].compile(); err != nil {
			return z.Err(err, "rewrite[%d]", i)
		}
	}
	return nil
}
```

## 동시성 & 상태

2-tier 워커 풀, 둘 다 config로 bound.

- **Tier 1 (jobs)**: `Warmer`가 `jobs chan *Job`(`warm.queue_size` 버퍼)와 `warm.jobs`개의 goroutine을 소유. `POST /v1/job`는 검증·Store 생성 후 non-blocking enqueue; 버퍼가 차도 `202` state=`pending`으로 반환(worker가 집어감) — 핸들러가 막히지 않는다.
- **Tier 2 (layers)**: 한 job 안에서 각 `PlannedLayer`의 `Source.Warm`을 `warm.concurrency` 크기 semaphore(`chan struct{}`) 아래 실행 — 큰 이미지 하나가 대역폭을 채우되 다른 job을 굶기지 않게.
- **dedup**: Store가 active job을 `(ref + 정렬된 platform set)`으로 키잉. `POST`는 같은 키의 active job이 있으면 enqueue 대신 그걸 `200`으로 반환. **이유**: nginx를 데우는 fleet의 N개 클라이언트가 upstream을 N번 두드리면 안 된다. Nomad/k3s reconcile loop가 반복 호출해도 안전. (mutable tag 주의: dedup은 active job 동안 tag를 stable로 취급 — mid-job에 재push된 tag는 첫 job이 끝날 때까지 두 번째 warm을 안 띄운다. 문서화.)
- **cancellation**: 각 Job은 server base ctx에서 파생한 child ctx를 받고 `cancel`을 보관. `DELETE`가 호출 → `remote.WithContext(ctx)`와 daemon client가 전파해 in-flight blob copy가 즉시 abort.
- **progress는 lock-free**: byte counter는 atomic이라 status handler가 직접 읽고 copy loop와 경합 없음. Store map만 state transition 시 `RWMutex` 하에 mutate. `Snapshot()`이 scalar 복사 + atomic load로 JSON 응답을 만듦.
- **fan-out**: job이 `warm`에 도달하면 Warmer가 Target들을 동시에(target당 goroutine) 호출. 한 Target 실패는 그 `TargetProgress`만 failed로 두고 warm 자체나 다른 target을 실패시키지 않음(cache는 이미 hot).
- **Store interface**: in-memory `memStore`(map + RWMutex + 완료 job TTL eviction). 인터페이스이므로 phase-2에 bbolt/sqlite가 API 레이어를 안 건드리고 drop-in. (재시작 시 job history 소실은 warm cache 용도에선 수용 가능 — audit이 필요하면 phase-2.)

**otel은 first-class surface**(root.go가 이미 otx를 wire 했으니 적극 활용): worker loop 주변에 UpDownCounter `gantry.warm.jobs.active`, byte counter `gantry.warm.bytes`, histogram `gantry.warm.duration`, fan-out counter `gantry.downstream.fanout`. job/fanout마다 `otx.From(ctx)`에서 딴 span. `log.From(ctx)`(slog)로 phase 로깅.

## serve 커맨드 & 생명주기

`serve`를 `NewCmdRoot().Commands`에 version/config/greet 옆에 추가(`cmd/serve.go`의 `NewCmdServe()`). root.go의 `OnRunPass` chain이 이미 `version` 외 모든 subcommand에 대해 `UseConfigInit`(config + otx를 ctx에 주입)를 돌리고 `o.Shutdown`을 defer 하므로, serve 핸들러는 greet.go처럼 `c := use_config.Must(ctx)`를 그냥 가정하면 된다 — root에 새 wiring 불필요.

핸들러 안 조립·실행 순서:
1. `c := use_config.Must(ctx)`; `flg.VisitP(cmd, "addr", &c.Serve.Addr)`(greet.go의 `--format`과 동일하게 `--addr`로 yaml override).
2. `src := warm.NewGGCRSource(c.Serve.Registry)`.
3. `targets, err := down.NewRegistry(c.Serve.Targets)` — factory가 각 docker/containerd socket을 dial, 도달 불가 target은 log+skip 하고 `/v1/target`에서 `ready:false`로 노출.
4. `store := warm.NewMemStore()`; `wmr := warm.NewWarmer(src, targets, store, c.Serve.Warm)`; `wmr.Start(ctx)`가 worker pool을 ctx에 bind 해 띄움.
5. `h := server.New(wmr, store, targets)`를 `server.Auth(c.Serve.Auth)` 미들웨어로 감싼다 — `/v1/*`는 `Authorization: Bearer` token 화이트리스트 또는 검증된 mTLS client cert 중 하나면 통과(OR), `/healthz`는 bypass. `srv := &http.Server{Addr: c.Serve.Addr, Handler: h, BaseContext: func(net.Listener) context.Context { return ctx }}`. mTLS면 `srv.TLSConfig`에 `ClientCAs` + `ClientAuth: VerifyClientCertIfGiven` 설정.
6. TLS cert가 설정됐으면 `go srv.ListenAndServeTLS(cert, key)`, 아니면 `go srv.ListenAndServe()`(reverse-proxy가 TLS 종단하는 경우); `<-ctx.Done()`(xli가 SIGINT를 root ctx로 전파)로 block.

**graceful shutdown**: ctx cancel 시 `shutdownCtx, _ := context.WithTimeout(context.Background(), time.Duration(c.Serve.ShutdownGrace))`로 `srv.Shutdown(shutdownCtx)`(in-flight HTTP·SSE drain) → `wmr.Stop()`(jobs chan close, 남은 job ctx cancel, WaitGroup join) → `targets.Close()`(daemon client close). otel flush는 root.go가 deferred한 `o.Shutdown`이 마지막에 처리. 에러는 `z.Err`로 wrap, `log.From(ctx).Info("serving", slog.String("addr", c.Serve.Addr))`. 모든 local은 snake_case. listener는 전부 stdlib이라 static scratch 바이너리에 추가 런타임 불필요(단 HTTPS cache나 tcp:// daemon TLS를 쓰면 scratch에 `ca-certificates.crt`를 COPY 해야 x509 에러를 피함 — buildx bake 파이프라인에 추가).

## 구현 순서 (단계별)

Phase-1만. 각 마일스톤은 독립적으로 컴파일·테스트 가능하게.

- [ ] **M0 — config 골격**: `cmd/config/duration.go`(named Duration + TextMarshaler), `cmd/config/serve.go`(ServeConfig/AuthConfig/RegistryConfig/WarmConfig/TargetConfig), `cmd/config/rewrite.go`(RewriteRule + BytesUnmarshaler + compile), `Config`에 `Serve` 추가 + `Evaluate`(기본값 + rewrite 컴파일). `gantry.yaml`에 serve 블록. yaml decode 라운드트립 + rewrite `{glob: template}` 디코드/컴파일 테스트.
- [ ] **M1 — 도메인 + Store**: `internal/warm/job.go`(Job/LayerProgress/TargetProgress/Plan/Caps), `Store` interface + `memStore`(map+RWMutex, TTL eviction, Snapshot). Store 단위 테스트(race detector).
- [ ] **M2 — Source(warm)**: `warm.Source` interface + `internal/warm/rewrite.go`(doublestar 매칭 + template 렌더 + identifier append; CacheRef 산출) + `Resolve`(공통: `remote.Get`, index/image 분기, platform 필터로 `nil`/`unknown` skip, `Plan` 산출). **`source_copy.go`(기본)**: upstream pull → HEAD skip → `remote.WriteLayer` push, manifest/index push, per-blob countingLayer. **`source_proxy.go`**: cache read→`io.Copy(io.Discard, countingReader)` EOF. `registry.mode`로 factory 선택. insecure transport/authn(cache) + DefaultKeychain(upstream). 로컬 registry against copy+proxy 양쪽 byte 카운팅 테스트.
- [ ] **M3 — Warmer 엔진**: 2-tier worker pool(jobs chan + layer semaphore), dedup 키, cancellation, atomic 진행률, state machine. `Warmer.Run`이 Source 구동 후 (M5 전까지는 fan-out 스텁). 동시성 테스트.
- [ ] **M4 — HTTP 서버**: `internal/server`(ServeMux method+path, JSON helper, `POST/GET/DELETE /v1/job`, `GET /v1/job/{id}`, `/healthz`). `auth.go` 미들웨어(bearer token constant-time 비교 + optional mTLS, `/healthz` bypass) + `AuthConfig` env 확장. 그다음 `/v1/job/{id}/progress` SSE(Flusher) + `?wait=` long-poll. 핸들러·인증 테스트(httptest, 401/200 경로).
- [ ] **M5 — down seam(docker)**: `internal/down/down.go`(Target+Verifier/Collector+Capabilities), `factory.go`, `docker.go`(v28 client, ImagePull→jsonmessage drain→sink, base64url auth, Ping). `GET /v1/target`, `POST /v1/target/{name}/pull`. Warmer fan-out 연결.
- [ ] **M6 — down seam(containerd)**: `containerd.go`(v2 client, `WithNamespace(k8s.io)`, `WithPullUnpack`, ContentStore polling 또는 state-level 진행률, Version). multi-arch index pull.
- [ ] **M7 — 관측성 & 마감**: otel meter(`gantry.warm.bytes/duration/jobs.active`, `gantry.downstream.fanout`) + job/fanout span, `serve` graceful shutdown 완성, Dockerfile에 `ca-certificates.crt` COPY, README/예제 config.

> **Phase-2가 추가할 것**(설계만 끝, 미구현): (1) image **signature 검증** — `down.Verifier`(cosign/sigstore)를 Warmer의 warm→fanout 사이 optional 훅으로, `verify:true` capability 자동 노출. (2) 각 등록된 docker/containerd의 image **GC** — `down.Collector` + `POST /v1/target/{name}/gc` + 스케줄 루프. 둘 다 `internal/down`(+그 config)에 격리되어 phase-1 타입/엔드포인트 무변경.

## 미해결/결정 필요

**결정됨** (2026-06-30):
- ✅ **warm-set = API 전용**. 데울 이미지는 `POST /v1/job`로만 받는다(부팅 시 config 선언형 auto-warm 없음). Nomad/k3s reconcile loop·CI가 호출하는 모델이고, dedup이 중복 호출을 흡수한다.
- ✅ **platform = 요청 인자 primary**. caller가 `platforms`로 지정; 생략 시 `WarmConfig.Platforms`(설정 fallback) → 호스트 arch. 우선순위: **request.platforms > WarmConfig.Platforms > 호스트 GOOS/GOARCH**.
- ✅ **persistence = in-memory만**. `memStore`(map+RWMutex+TTL eviction); 재시작 시 job history 소실 수용. `Store` interface로 두어 phase-2에 bbolt/sqlite drop-in.
- ✅ **API 인증 = 토큰/mTLS 미들웨어 포함**. `/v1/*`는 bearer token 화이트리스트 OR 검증된 mTLS client cert(`/healthz` 면제). token은 constant-time 비교, `${ENV}` 확장 지원.
- ✅ **워밍 = Source 2구현, copy 기본**. `copySource`(upstream→cache push, HEAD skip로 증분 추적) 기본 + `proxySource`(read→discard self-fill). `registry.mode`로 선택. copy는 cache writable 필요, proxy는 read-only proxy용.
- ✅ **ref 매핑 = config rewrite 규칙**. `registry.rewrite`에 `{glob: template}` 순서 배열(first-match) — source ref → cache ref. copy push-dest / proxy pull-src / downstream trigger를 한 규칙으로 통일. API는 항상 canonical upstream ref만 받음.
- ✅ **rewrite 기본 규칙 = `{{.CacheHost}}/{{.Repo}}`**. 빈 설정 시 이 1줄 주입(짧고 단순). 멀티 upstream에서 repo path 충돌이 우려되면 `{{.CacheHost}}/{{.Registry}}/{{.Repo}}`로 명시 override.
- **distribution 시 per-target platform 적용**: warm은 request.platforms로 끝나지만, downstream 트리거는 index digest로 보내 각 daemon이 자기 arch를 고르게 한다(현재안). `TargetConfig.Platforms`(arm64-only 노드 등) override를 어떤 규칙으로 교차시킬지 — 단순히 "target이 못 받는 arch면 fan-out skip"으로 둘지 확정.
- **containerd namespace 기본값**: `k8s.io`로 고정(k3s 가정)할지, target별 필수 입력으로 둘지. 기본값을 두면 default namespace로 받아 kubelet에 안 보이는 사고를 막지만, 비-k3s 환경에선 오해 소지.
- **downstream pull 진행률 입도**: containerd byte-level polling(ContentStore)을 phase-1에 넣을지, state-level(once-at-complete)로 출시하고 phase-2로 미룰지. docker는 byte-level 확정.

---

## 구현 현황 (2026-06-30)

Phase-1 전부 구현·검증 완료. 약 2,900 LOC + 테스트, race/vet clean.

- [x] **M0 config** — `cmd/config/{serve,duration,rewrite}.go`, Evaluate 기본값·rewrite 컴파일. RewriteRule `{glob: template}` 디코드/마샬 round-trip.
- [x] **M1 도메인+Store** — `internal/warm/{job,store}.go`. atomic 진행률, `memStore`(dedup/TTL/snapshot), race 동시성 테스트.
- [x] **M2 Source** — `rewrite.go`(doublestar+template+identifier), `source_copy.go`(HEAD-skip delta), `source_proxy.go`(read-through), multi-arch 필터·filtered index commit. in-mem registry 테스트.
- [x] **M3 Warmer** — 2-tier worker pool, dedup, cancellation, queue-full backpressure, state machine. end-to-end 테스트.
- [x] **M4 서버** — stdlib ServeMux `/v1/job` CRUD + SSE/long-poll + `/v1/target` + `/healthz`, bearer/mTLS auth(env 확장). httptest + 실바이너리 smoke.
- [x] **M5 docker seam** — `internal/down/{down,factory,docker,distributor}.go`. **실제 docker 데몬으로 전체 루프 검증**: warm(hello-world: hub→registry copy) → docker가 cache에서 pull → `bytes_done=bytes_total`, target=pulled.
- [x] **M6 containerd seam** — `containerd.go`(v2 client, namespace, WithPullUnpack). 데몬독립 단위테스트 통과; 실 Pull은 reachable containerd 소켓(k3s 등)에서 검증 예정.
- [x] **M7 마감** — Warmer otel meter(`gantry.warm.bytes/duration/jobs.active`) + job span, Dockerfile `ca-certificates.crt`, README.

**검증 안 된 것**: containerd 실 Pull(소켓 없음), copy↔downstream insecure split-brain(데몬 insecure 정책은 데몬측 설정 — 실험에서 `127.0.0.0/8`만 신뢰 확인). **Phase-2 예정**: `down.Verifier`(cosign) / `down.Collector`(GC) — capability interface로 추가, phase-1 코드 무변경.

---

## v2 — store 모델로 일반화 (2026-06-30)

phase-1 구현(M0–M7) 후, "warm + downstream trigger"를 **store 간 이미지 이동**으로 일반화함. downstream pull도 per-layer 추적이 되면서 cache fill(remote→cache)과 같은 모양(`Transfer`)이 되기 때문.

- **store** = 이미지 저장소. kind `registry`(gantry가 blob read/write) 또는 `docker`/`containerd`(gantry가 pull trigger). config의 `registry`+`targets`가 단일 `serve.stores`로 통합. `allow_unknown_stores`(기본 false)로 미선언 registry 호스트 허용.
- **job** = `{ref, source, target, distribute}`: `source`(registry)→`target`(registry) copy 후 `distribute`(engine)들이 pull. source/target은 store 이름 또는 bare host(free-form).
- **Transfer** = 한 이동 단계의 진행률(`{store, kind, source, ref, state, bytes_total, bytes_done, layers[]}`). cache copy와 각 engine pull이 전부 같은 모양. 응답은 `job.transfers[]`로 통일.
- **per-layer 진행률**: registry copy는 gantry가 직접 카운트(정확). engine pull은 docker jsonmessage / containerd ContentStore 폴링으로 best-effort(데몬이 byte를 보고할 때만; docker 29 containerd-store는 빠른 로컬 pull 시 byte 미보고 → state-level).
- **API**: `GET /v1/store`(전체 store + capability + readiness), `POST /v1/job`(source/target/distribute), `GET /v1/job/{id}`(transfers[]), `POST /v1/store/{name}/pull`(수동).
- **패키지**: `internal/store`(해석+status), `internal/down`(engine: docker/containerd, Sink), `internal/warm`(registry Source copy/proxy + Warmer transfers 오케스트레이션). 실 docker+containerd로 e2e 검증.

위 §HTTP API / §도메인 모델 / §설정은 v1(warm-centric) 기준이며, 실제 구현은 이 v2 모델을 따른다.
