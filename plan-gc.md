# gantry phase-2 설계 — 이미지 retention / GC

## 개요

엣지 use case는 한 줄로 요약된다: 사용자가 한 이미지의 여러 **태그**를 받아서 돌려보다가, 하나로 정착한다. 그래서 GC는 단순한 "오래된 것 삭제"가 아니라 (1) 정말 오래 안 쓴 것 삭제, (2) repo별로 최근에 써본 태그 N개 보존, (3) 정착한 태그 pin, (4) 컨테이너가 쓰는 중인 이미지는 절대 삭제 안 함 — 이 네 정책을 동시에 만족해야 한다.

핵심 문제는 docker도 containerd도 **이미지 last-used 시각을 노출하지 않는다**는 것이다(검증 완료). 따라서 gantry가 직접 그 신호를 만들어 보존해야 한다. 설계는 두 부분으로 갈린다:

1. **usage-watcher** — 엔진별 long-lived goroutine이 daemon의 event stream(docker `cli.Events` / containerd `client.Subscribe`)을 구독해 "컨테이너가 떴다 = 그 이미지가 쓰였다"를 잡아 index에 `lastUsed=now`로 stamp한다. 시작 시점엔 기존 컨테이너를 enumerate해서 cold-start seed한다. event엔 backlog가 없고 daemon은 과거를 모르므로, index는 **disk에 persist**되어 gantry 재시작을 넘어 살아남는다 — gantry가 last-used의 유일한 source of truth다.
2. **policy engine** — GC 요청 시 index를 한 번 읽고, 위 네 정책을 순서대로 적용해 delete set을 만든다. dry-run이 먼저(후보 + 사유 반환), confirm 시 apply. 삭제는 기존 `down.Collector` capability 뒤에 붙는다.

저장소는 `go.etcd.io/bbolt`(pure-Go, CGO_ENABLED=0 clean, FROM-scratch 정적 바이너리에 그대로 링크, 유일한 transitive dep인 `golang.org/x/sys`는 이미 indirect로 vendored). 기존 in-memory job `Store`(internal/warm/store.go)는 그대로 두고 별개로 둔다 — job record는 휘발성이어도 되지만 retention index는 영속이어야 한다.

---

## 1. 기록할 정보 (data model)

엔진별 · 이미지-ref별로 하나의 `Record`를 둔다. key는 **daemon-local image reference**(태그 포함), 즉 warm job이 distribute한 그 ref와 같다. 태그 단위로 record를 두므로 "keep N tags per repo"가 그냥 repo prefix로 묶어 시간 정렬하는 것으로 환원된다.

```go
// internal/retention/record.go
package retention

import "time"

// Record is the per-(engine, image-ref) retention state. The bbolt key is Ref
// itself; Ref is duplicated into the value so a bucket scan is self-describing.
type Record struct {
	Ref             string    `json:"ref"`              // full local ref incl. tag, e.g. "cache.local/team/app:1.2"
	Repo            string    `json:"repo"`             // name.Context().RepositoryStr(), the keep-N grouping key (stored to avoid re-parsing on every GC)
	Tag             string    `json:"tag"`              // tag portion (for pin-pattern matching + display); "" for digest refs
	Digest          string    `json:"digest,omitempty"` // resolved manifest digest (go-digest string); identity for retag + safe delete-by-digest

	LastUsed        time.Time `json:"last_used"`        // THE last-USED signal gantry maintains; zero => never observed used
	LastDistributed time.Time `json:"last_distributed"` // set when a warm Job's engine-pull for this (engine,ref) completes; LastUsed fallback on cold-start gap
	FirstSeen       time.Time `json:"first_seen"`

	Pinned          bool      `json:"pinned,omitempty"`      // explicit pin on this exact ref
	PinPattern      string    `json:"pin_pattern,omitempty"` // doublestar pattern that pinned it (empty if pinned by exact ref)
}
```

`lastUsed` / `lastDistributed` / `pinned` / `digest` / `repo` / `tag` 가 전부 한 record에 들어간다.

**"keep N most-recent tags per repo" 표현** — 별도 자료구조가 필요 없다. policy 평가 시 한 엔진의 모든 record를 `Record.Repo`로 group-by 한 뒤, 각 group을 `LastUsed` desc로 정렬해 상위 N개를 force-keep 한다. N은 정책 파라미터(`KeepN`)다. record에 미리 저장하는 건 group key인 `Repo` 뿐.

**pinning 표현** — 두 형태를 한 record로 흡수한다. 정확한 ref pin은 `Pinned=true`. pattern pin(예: `prod-*`, `*:latest`)은 정책 본문의 pin 리스트로 들고 다니다가 평가 시 `Tag`/`Ref`에 `doublestar.Match`로 적용한다(`bmatcuk/doublestar/v4`는 이미 vendored — internal/warm/rewrite.go 사용 중). pattern으로 매칭돼 보호된 record엔 감사를 위해 `PinPattern`을 stamp해 둘 수 있다. pin 리스트 자체(영속 pin 집합)는 아래 meta bucket에 별도 보관한다.

정책 파라미터는 config과 API body 양쪽에서 같은 모양으로 쓴다:

```go
// internal/retention/policy.go
type Policy struct {
	MaxAge config.Duration `json:"max_age" yaml:"max_age"` // lastUsed가 이보다 오래된 것 삭제 후보
	KeepN  int             `json:"keep_n"  yaml:"keep_n"`  // repo별 최근 사용 태그 N개 보존
	Pins   []string        `json:"pins"    yaml:"pins"`    // 정확 ref 또는 doublestar 패턴
}
```

---

## 2. 사용 신호 수집 (usage-watcher)

신호의 의미는 "컨테이너가 **떴다** = 그 이미지가 지금 쓰였다"이다. `create`가 아니라 **start**를 쓴다 — create는 컨테이너가 실제로 안 떠도 발생하므로 오탐을 만든다.

엔진별로 long-lived goroutine 하나(`Watcher`)가 돈다. event를 받을 때마다 해당 ref의 record에 `LastUsed = Envelope/Message timestamp`로 Put 한다. event엔 backlog가 없으므로 watcher 시작 직전에 cold-start seed를 한 번 돌린다.

### docker

구독은 서버측 필터로 좁힌다(엣지 자원 절약 — network/volume/image/exec_* 이벤트를 디코드하지 않게):

```go
f := filters.NewArgs()
f.Add("type", "container")   // container 이벤트만 (events.ContainerEventType, api/types/events/events.go:12)
f.Add("event", "start")      // ActionStart  (events.go:28)
f.Add("event", "restart")    // ActionRestart(events.go:29) — 재기동도 touch
f.Add("event", "unpause")    // ActionUnPause(events.go:33)
msgs, errc := cli.Events(ctx, events.ListOptions{Filters: f}) // client/events.go:18 -> (<-chan events.Message, <-chan error)
```

event → image 해소(검증: daemon/events.go:25-34): docker는 task event와 달리 이미지 ref를 **인라인으로** 준다.

- `msg.Type == events.ContainerEventType` 그리고 `msg.Action == events.ActionStart` 등.
- `msg.Actor.ID` = 컨테이너 ID (이미지 아님).
- **`msg.Actor.Attributes["image"]`** = `container.Config.Image` = 요청된 이미지 **ref(태그 레벨)**, 예 `cache.local/team/app:1.2`. 이게 index key. (resolved sha256 image ID 아님 — 정확히 gantry가 원하는 태그 단위 granularity.)
- timestamp는 `msg.TimeNano`(나노초; `msg.Time`은 초 단위, events.go:130-131) → `index.Touch(engine, attrs["image"], time.Unix(0, msg.TimeNano))`.
- `msg.From` / `msg.ID`는 위 둘의 deprecated alias(events.go:119-122) — 쓰지 않는다.

`cli.Events`는 long-lived stream이라 decode 에러/끊김 시 멈춘다(client/events.go 주석 :13-16). `errc`를 받으면 backoff 후 재구독하는 reconnect loop로 감싼다. 재구독 시 gap 동안 놓친 사용을 잃지 않도록 cold-start seed를 한 번 더 merge한다(max로).

### containerd

```go
envs, errc := cli.Subscribe(ctx,
	`topic=="/containers/create"`, // ContainerCreate — 이미지 인라인
	`topic=="/tasks/start"`,       // TaskStart — 진짜 RUN 신호, 이미지 없음(해소 필요)
) // client/client.go:675 -> (<-chan *events.Envelope, <-chan error)
```

`Envelope`(core/events/events.go:25-30): `Timestamp time.Time`, `Namespace string`, `Topic string`, `Event typeurl.Any`. 디코드는 `typeurl.UnmarshalAny(e.Event)`(패턴: cmd/ctr/commands/events/events.go:56).

topic별 해소:

- **`/containers/create`** → `*events.ContainerCreate`. **이미지 인라인** — `ev.GetImage()`(api@v1.11.1/events/container.pb.go:89, proto field `image=2`)가 ref. lookup 불필요. `index.Touch(engine, ev.Image, env.Timestamp)`.
- **`/tasks/start`** → `*events.TaskStart`. `ContainerID`만 있고 이미지 없음(api@v1.11.1/events/task.proto). 진짜 "지금 RUN 중" 신호이므로 받되, container_id → image 해소가 필요하다:

```go
ctx := namespaces.WithNamespace(ctx, env.Namespace) // 이벤트는 namespaced
c, err := cli.ContainerService().Get(ctx, ev.ContainerID) // -> containers.Container, .Image (core/containers/containers.go:44)
if err == nil && c.Image != "" {                          // r.Image=="" guard (client/container.go:217)
	index.Touch(engine, c.Image, env.Timestamp)
}
```

- timestamp는 항상 `Envelope.Timestamp`(서버측 event time) — buffered/replayed event도 순서가 유지된다. `time.Now()` 쓰지 않는다.
- 다중 namespace를 서비스하면 `cli.NamespaceService().List(ctx)`로 namespace를 돌며 각각 구독/seed 한다(엔진 config의 `Namespace`가 기본값).

### cold-start seeding

watcher 시작 직전, 이미 떠 있거나 존재하는 컨테이너를 enumerate해서 그 이미지를 "최근에 쓴 것"으로 seed한다. 안 그러면 정착해서 돌고 있는 이미지가 "한 번도 안 쓴 것"으로 보여 잘못 GC된다.

docker:

```go
cs, _ := cli.ContainerList(ctx, container.ListOptions{All: true}) // client/container_list.go:14, All=true는 stopped 포함 (options.go)
for _, c := range cs { // container.Summary (api/types/container/container.go:124-136)
	t := time.Unix(c.Created, 0) // .Created(int64, unix sec) — 유일한 timestamp; 보수적 lower-bound
	index.Seed(engine, c.Image, t)                 // .Image = ref(태그), event의 Attributes["image"]와 동일 key
	if c.State == container.StateRunning {          // 실행 중이면 used-now로 승격
		index.Touch(engine, c.Image, now)
	}
}
```

containerd:

```go
ctx = namespaces.WithNamespace(ctx, ns)
cl, _ := cli.Containers(ctx) // client/client.go:326
for _, c := range cl {
	info, _ := c.Info(ctx) // containers.Container
	if info.Image == "" { continue }
	t := info.CreatedAt
	if info.UpdatedAt.After(t) { t = info.UpdatedAt } // max(CreatedAt, UpdatedAt) = 약간 더 신선한 추정
	index.Seed(engine, info.Image, t)
}
```

`Seed`는 persisted 값 위에 **max로** merge한다(`record.LastUsed = max(stored, seedTime)`). `Touch`는 무조건 갱신(항상 더 최신). 둘 다 `FirstSeen`이 zero면 채운다.

**한계(미해결로 연결)**: cold-start scan은 *아직 존재하는* 컨테이너만 복구한다. 떠봤다가 삭제된 trial 컨테이너의 사용 기록은 — 그 TaskStart event를 watcher 가동 전에 놓쳤다면 — 오직 persisted index에만 남는다. 그래서 disk persistence가 필수다.

### distribute에서의 stamp (watcher 보완)

warm `Warmer.runDistribute`(internal/warm/warm.go:367)에서 엔진 pull이 완료될 때마다 `index.Distributed(engine, ref, now)`를 호출한다: `LastDistributed=now`로 set하고, `LastUsed.IsZero()`이면 `LastUsed=now`도 set(갓 받아서 돌려본 태그는 방금 쓴 것으로 친다). 이게 cold-start gap을 메우는 두 번째 신호다.

---

## 3. 저장소 (bbolt)

**선택: `go.etcd.io/bbolt`** (pure-Go embedded B+tree KV). 이유:

- CGO_ENABLED=0 / FROM-scratch 정적 바이너리 hard 제약을 만족하는 유일한 후보다. 유일한 transitive dep `golang.org/x/sys`는 이미 indirect로 vendored(go.mod:78) → 의존성 트리에 사실상 아무것도 안 더하고 바이너리에 1MB 미만 추가. modernc.org/sqlite는 transpiled libc로 수 MB를 끌어와 엣지 이미지에 부적합.
- crash-safety가 결정타다. bbolt는 copy-on-write + meta-page 2-phase commit이라 엣지 디바이스 전원 차단 시 최악이 직전 in-flight 트랜잭션 손실, index 전체 손상은 없다. 단일 JSON 파일은 usage event마다 전체 rewrite(O(N) write amplification + fsync)고, in-memory snapshot은 마지막 snapshot 이후 event를 통째로 잃어 재부팅 후 lastUsed가 퇴행 → 잘못된 GC.
- write 패턴이 정확히 맞다. usage event = 짧은 `Update()` 안의 작은 Put 하나(전체 rewrite/SQL 없음). GC = 한 엔진 sub-bucket을 `ForEach`로 도는 read-only txn 하나.
- SQL 불필요. 유일한 질의는 repo group-by + 시간 정렬인데 수백 record를 Go에서 도는 게 trivial하다. repo 그룹핑은 `name.Context().RepositoryStr()`로 이미 공짜(internal/warm/rewrite.go).

**bucket / key 레이아웃** — 엔진별로 독립 scan되게 엔진을 sub-bucket으로:

```
DB file: <retention.path>  (default /var/lib/gantry/retention.db)

bucket "img"
  └─ sub-bucket <engineName>          // 설정된 엔진 store 하나당 하나
       key   = <ref>                  // "cache.local/team/app:1.2" (= distribute한 그 ref, 태그별 1 record)
       value = json(Record)

bucket "pin"
  └─ sub-bucket <engineName>
       key   = <ref-or-pattern>       // 영속 pin 집합 (정확 ref 또는 doublestar 패턴)
       value = json(PinEntry{By, At})

bucket "meta"
       key "schema_version" = "1"
```

값 인코딩은 `encoding/json`(엣지 박스에서 사람이 들여다보기 쉬움; record가 작아 gob 대비 비용 무시 가능). DB 경로는 config:

```go
// cmd/config/serve.go: ServeConfig에 추가
type RetentionConfig struct {
	Path     string          `yaml:"path"`      // bbolt 파일 경로; "" => GC 비활성(엔진은 Collector를 노출하지 않음)
	Interval Duration        `yaml:"interval"`  // optional 스케줄(아래 §5); "" => 수동만
	Policy   retention.Policy `yaml:"policy"`   // 기본 max_age/keep_n/pins
}
// ServeConfig.Retention RetentionConfig `yaml:"retention"`
```

기존 in-memory job `Store`(internal/warm/store.go)는 그대로 분리 유지 — job record는 `JobTTL` 후 버려도 되는 휘발성, retention index는 영속. 둘은 다른 lifecycle이라 같은 store에 섞지 않는다.

`Index`는 watcher와 policy engine이 공유하는 facade다:

```go
// internal/retention/index.go
type Index struct{ db *bbolt.DB }

func Open(path string) (*Index, error)                                  // bbolt.Open + 버킷 생성
func (ix *Index) Touch(engine, ref string, t time.Time) error           // LastUsed = max(cur, t) (보통 항상 갱신)
func (ix *Index) Seed(engine, ref string, t time.Time) error            // LastUsed = max(cur, t), FirstSeen 채움
func (ix *Index) Distributed(engine, ref string, t time.Time) error     // LastDistributed=t; LastUsed.IsZero()면 LastUsed=t
func (ix *Index) List(engine string) ([]Record, error)                  // 한 엔진 sub-bucket ForEach
func (ix *Index) Delete(engine, ref string) error                       // GC apply 시 record 제거
func (ix *Index) Pin(engine, refOrPattern, by string) error             // "pin" 버킷 Put
func (ix *Index) Unpin(engine, refOrPattern string) error
func (ix *Index) Pins(engine string) ([]PinEntry, error)
```

`Touch`/`Seed`/`Distributed`는 `Ref` 첫 등장 시 `Repo`/`Tag`/`Digest`를 한 번 파싱해 채운다(`name.ParseReference` → `Context().RepositoryStr()`, `Identifier()`).

---

## 4. 보존 정책 평가 (policy engine)

GC 한 번 = 한 엔진의 sub-bucket을 read-txn으로 `List`한 뒤, 아래 순서대로 평가해 `[]Candidate`(삭제 대상 + 사유)를 만든다. dry-run은 여기서 멈추고 반환, apply는 delete를 실행한다.

```go
type Candidate struct {
	Ref      string `json:"ref"`
	Digest   string `json:"digest,omitempty"`
	LastUsed time.Time `json:"last_used"`
	Reason   string `json:"reason"` // "age_exceeded"
}
type Decision struct {
	Delete []Candidate `json:"delete"`
	Keep   []Kept      `json:"keep"` // ref + 보호 사유: "in_use" | "pinned" | "keep_n_recent" | "within_max_age"
}
```

평가 순서(보호가 삭제를 이긴다):

```
func Evaluate(now time.Time, recs []Record, inUse map[string]bool, p Policy) Decision
 1. PROTECT in-use   — live 컨테이너가 참조 중인 ref/digest는 inUse 집합에 있으면 무조건 keep("in_use").
                       inUse는 index가 아니라 GC 시점에 LIVE로 재조회(아래).
 2. PROTECT pinned   — Record.Pinned == true, 또는 Ref/Tag가 p.Pins의 어떤 정확 ref·doublestar 패턴과
                       매칭되면 keep("pinned").
 3. PROTECT keep-N   — 남은 record를 Record.Repo로 group-by; 각 group을 LastUsed desc 정렬(tie-break: Ref);
                       상위 p.KeepN개를 keep("keep_n_recent"). 개별적으로 max_age를 넘었어도 보존된다 —
                       "여러 태그 돌려보고 정착" 시 최근 돌려본 집합을 지킨다.
 4. DELETE by age    — 남은 것 중 effLastUsed(r) 나이가 p.MaxAge를 넘으면 delete("age_exceeded").
                       나머지는 keep("within_max_age").
```

cold-start gap 처리 — `effLastUsed`:

```go
func effLastUsed(r Record) time.Time {
	switch {
	case !r.LastUsed.IsZero():        return r.LastUsed        // 진짜 last-used 신호
	case !r.LastDistributed.IsZero(): return r.LastDistributed // fallback: 받았으나 사용 관측 없음
	default:                          return r.FirstSeen       // 최후: 처음 본 때
	}
}
```

이렇게 하면 watcher 가동 전·짧은 가동 시간 때문에 last-used 이력이 비어 있어도, distribute 시각(혹은 first-seen)으로 나이를 재서 갓 받은 것을 즉시 삭제하지 않는다.

in-use 집합은 **index가 아니라 GC 시점 LIVE 재조회**(정책 #4는 비협상):

```go
// docker: 컨테이너가 참조 중인 ref + sha
cs, _ := cli.ContainerList(ctx, container.ListOptions{All: false}) // running만
for _, c := range cs { inUse[c.Image] = true; inUse[c.ImageID] = true }
// 보조: cli.ImageList(image.ListOptions{ContainerCount: true})의 Summary.Containers > 0 (api/types/image/summary.go:14)

// containerd:
cl, _ := cli.Containers(namespaces.WithNamespace(ctx, ns))
for _, c := range cl { if info, e := c.Info(ctx); e == nil { inUse[info.Image] = true } }
```

삭제(apply) — 후보별로 엔진 delete를 부르고, **같은 의미의 bbolt Delete**로 index를 동기화한다:

```go
// docker
res, err := cli.ImageRemove(ctx, cand.Ref, image.RemoveOptions{Force: false, PruneChildren: true}) // client/image_remove.go:12
// Force:false => 아직 참조 중이면 force-yank 대신 에러. 결과 res는 {Untagged|Deleted} 엔트리(delete_response.go:8-12).
// 한 ref 제거는 보통 {Untagged}만 — content(Deleted)는 마지막 태그가 빠질 때만 free.
// containerd
err := cli.ImageService().Delete(ctx, cand.Ref, images.SynchronousDelete()) // core/images/image.go:96,69
// SynchronousDelete: 레코드 제거 후 ScheduleAndWait로 GC를 즉시 돌려 blob/snapshot 회수(공유 레이어는 유지).
// 성공 시 ix.Delete(engine, cand.Ref) — index와 daemon 상태 일치.
```

docker는 한 태그 제거가 `{Untagged}`만 내고 디스크가 안 줄 수 있음(content는 마지막 참조 태그 제거 시 free; keep-N 정책이 이 사실을 흡수). containerd는 Synchronous GC가 그 이미지가 유일 참조였던 레이어/스냅샷만 회수.

### 4b. 엣지 use case 적합성

"여러 태그를 받아 돌려보다 하나로 정착":

- **keep-N**: trial 직후 돌려본 태그들은 `LastUsed`가 최신이라 repo group 상위 N에 들어 보호된다 — 많이 받아봐도 최근 시도분이 안 날아간다.
- **max_age**: 오래전에 받고 더는 안 돌린 버려진 태그는 age로 삭제된다 — 정착 후 잊힌 trial 정리.
- **pin**: 정착한 태그를 pin하면 max_age·keep-N과 무관하게 영구 보존.
- **in-use guard**: 지금 컨테이너가 돌리는 이미지는 어떤 정책으로도 안 지워진다 — 정착 이미지가 돌고 있으면 이중으로 안전.

---

## 5. API & capability

`down.Collector`(internal/down/down.go:44 `Collect(ctx) error`)에 retention을 얹는다. docker/containerd 엔진이 `Index`를 들고 있으면(retention.path 설정 시) `Collect`/dry-run/pin 메서드를 추가로 구현해 `Collector`를 만족 → `store.StoreStatuses`가 자동으로 `gc:true`를 보고한다(이미 `down.Capabilities` 타입 어서션으로 발견; down.go:56). 그 외 phase-1 코드는 안 바뀐다.

엔드포인트(stdlib `net/http`, 기존 `/v1/store/{name}/...` 라우팅과 동형):

```
GET  /v1/store/{name}/gc            # dry-run: 정책 평가만, Decision 반환 (삭제 안 함)
POST /v1/store/{name}/gc            # apply: body 정책으로 평가 후 delete set 실행, 회수 결과 반환
GET  /v1/store/{name}/pin           # 영속 pin 목록
POST /v1/store/{name}/pin           # {"ref":"...", "pattern":"..."} 중 하나로 pin 추가
DELETE /v1/store/{name}/pin         # pin 제거
```

GET/POST `/gc` body(없으면 config의 `retention.policy` 기본값):

```jsonc
{ "max_age": "720h", "keep_n": 3, "pins": ["*:stable", "cache.local/team/app:1.7"] }
```

응답:

```jsonc
// GET (dry-run): Decision 그대로
{ "delete": [ {"ref":"cache.local/team/app:1.2","reason":"age_exceeded","last_used":"..."} ],
  "keep":   [ {"ref":"cache.local/team/app:1.7","reason":"pinned"} ] }
// POST (apply): 실행 결과
{ "deleted": ["cache.local/team/app:1.2"], "untagged": ["..."], "errors": [], "evaluated": 14 }
```

핸들러 골격:

```go
func (s *Server) handleStoreGC(w http.ResponseWriter, r *http.Request) {
	eng, err := s.stores.Engine(r.PathValue("name"))
	if err != nil { writeErr(w, http.StatusNotFound, err.Error()); return }
	col, ok := eng.(down.Collector) // 또는 확장된 retention 인터페이스
	if !ok { writeErr(w, http.StatusNotImplemented, "store has no gc capability"); return }
	p := s.cfg.Retention.Policy
	if r.Body != nil { _ = readJSON(r, &p) } // body가 기본값 override
	dec, err := col.Plan(r.Context(), p)     // dry-run 평가
	if err != nil { writeErr(w, http.StatusBadGateway, err.Error()); return }
	if r.Method == http.MethodGet { writeJSON(w, http.StatusOK, dec); return }
	res, err := col.Apply(r.Context(), dec)  // confirm => 삭제
	if err != nil { writeErr(w, http.StatusBadGateway, err.Error()); return }
	writeJSON(w, http.StatusOK, res)
}
```

`down.Collector`는 무인자 `Collect(ctx)`(스케줄/cron용)와 `Plan`/`Apply`(API용)를 함께 노출하게 확장한다. `Collect`는 내부적으로 `Apply(ctx, Plan(ctx, configPolicy))`다.

**스케줄(optional)**: `retention.interval`이 설정되면 serve가 ticker goroutine을 띄워 각 GC-capable 엔진에 주기적으로 `Collect`를 돈다(엣지 무인 운용). watcher goroutine과 별개의 long-lived goroutine.

---

## 6. 구현 순서 (milestones)

각 단계가 독립적으로 테스트 가능하도록 watcher+index → policy → API 순서.

1. **`internal/retention` 코어 + bbolt Index** — `Record`, `Open`, `Touch`/`Seed`/`Distributed`/`List`/`Delete`/`Pin` 구현. **unit-testable**: tmp 파일에 bbolt 열고 Put/scan/merge(max) 검증. ref→Repo/Tag/Digest 파싱 검증. daemon 불필요.
2. **policy engine `Evaluate`** — pure 함수(`now, []Record, inUse, Policy` → `Decision`). **unit-testable**: 합성 record로 네 정책의 순서/경계(age 경계, keep-N tie-break, pin 패턴, in-use 우선)를 테이블 테스트. daemon 불필요. (가장 중요한 로직이라 먼저 굳힌다.)
3. **usage-watcher** — docker `cli.Events` reconnect loop + containerd `Subscribe` + event→image 해소 + cold-start seed. `Index.Touch` 호출. event 파싱/해소는 fake event로 unit-test, 실제 stream 소비는 **real daemon 필요**(internal/down의 기존 `*_integration_test.go` 패턴 따라 build-tag). cold-start seed의 `ContainerList`/`Containers` enumerate도 real daemon.
4. **distribute stamp 연결** — `Warmer.runDistribute`에서 엔진 pull 완료 시 `Index.Distributed` 호출. warm 테스트에 fake Index 주입해 검증.
5. **live in-use 재조회 + apply 삭제** — docker `ContainerList(All:false)`/containerd `Containers`로 inUse 수집, `ImageRemove`/`ImageService().Delete` 실행 후 `Index.Delete`. **real daemon 필요**(실제 삭제/회수 검증; DinD로 full loop).
6. **`down.Collector` 확장 + API** — `Plan`/`Apply`/`Collect`, `/v1/store/{name}/gc`·`/pin` 핸들러, `RetentionConfig` 배선, `StoreStatuses`가 `gc:true` 보고. 핸들러는 fake Collector로 unit-test, 통합은 real daemon.
7. **optional 스케줄** — `retention.interval` ticker goroutine. unit-testable(짧은 interval + fake Collector).

real daemon이 꼭 필요한 것: 3(stream 소비/seed enumerate), 5(실제 삭제), 6·통합. 나머지(1, 2, 4, 7 로직, 핸들러 분기)는 전부 unit-testable.

---

## 7. 미해결 / 결정 필요

- **keep_n 범위**: repo별 N(현재 설계) vs 전역 N vs 엔진별 N. repo별이 use case에 맞지만, repo가 아주 많은 엣지에서 총 보존량이 커질 수 있다 — repo별 + 전역 상한 조합이 필요한가?
- **pin 문법**: 정확 ref만 vs doublestar 패턴 허용(현재 둘 다). 패턴 pin은 강력하지만 실수로 광범위 매칭(`*`)해 GC를 무력화할 위험. 패턴 pin에 dry-run 미리보기를 강제할지.
- **persistence 경로 기본값**: `/var/lib/gantry/retention.db` 가정. FROM-scratch 컨테이너엔 이 경로가 volume mount여야 영속된다 — 기본값을 둘지, 미설정 시 GC 비활성으로 둘지(현재: path 미설정 → Collector 미노출).
- **watcher restart / backfill 한계**: gantry 다운 동안의 사용은 영구 소실(event backlog 없음, 컨테이너가 삭제됐으면 cold-start scan도 못 잡음). 다운타임이 길면 `effLastUsed` fallback이 distribute/first-seen으로 후퇴 — 잘못된 삭제를 막으려면 다운타임 후 첫 GC에 grace window(예: 재시작 후 1×max_age 동안 age 삭제 보류)를 둘지.
- **태그 다수 → 동일 digest**: 두 태그가 같은 content를 가리킬 때 keep-N을 태그 단위로 셀지 digest 단위로 셀지. 디스크 회수는 digest 단위(마지막 태그 제거 시)지만 사용자 의도는 태그 단위 — 현재 태그 단위 카운트.
- **삭제 원자성/부분 실패**: apply 중 일부 ref 삭제 실패 시 index를 어떻게 둘지(현재: 성공한 것만 `Index.Delete`, 실패는 다음 GC에서 재시도). daemon 삭제는 성공했는데 bbolt Delete가 실패하는 역순 케이스 처리.
- **in-use를 last-used로 승격하는 reconciler 주기**: event watcher 외에 주기적 in-use 스캔으로 `LastUsed=now`를 찍는 heartbeat를 둘지(돌고 있지만 start event를 놓친 컨테이너 보강). 둔다면 주기는?

### 7b. 후속 결정 — 이름 잃은 이미지(untagged) 리퍼 (2026-07)

docker 엔진에서 같은 태그가 갱신되면 이전 이미지는 태그만 잃고 `repo@digest`
참조와 함께 디스크에 영구히 남는다(containerd는 gantry가 retag 후 digest record를
지워 자체 GC가 회수하므로 해당 없음). 이를 다음과 같이 해소했다:

- **관측 시점 시계**: 태그를 잃은 정확한 시각은 알 수 없고 알 필요도 없다.
  인벤토리 스캔이 태그 없는 이미지를 처음 본 시각(`unt/<engine>/<id>`,
  write-once `first_seen`)부터 `retention.untagged_after`(docker 전용, 기본 1h,
  `"0s"`로 끔) 경과 후 삭제. 시작 grace window는 age GC와 동일하게 적용.
- **스캔은 GC 패스에 편승**: 별도 스케줄러 없이 `gcOnce`가 매 패스
  `Reconciler.Images`(ImageList)로 전체 인벤토리를 읽는다. 시작 시 1회 +
  usage/distribute poke + `interval` 유휴 wake로 eventual convergence를 얻는다.
  모르는 **태그 있는** ref도 이때 `Observe`(FirstSeen=now)로 seed되어 기존
  rule이 관리하게 된다(rule 없는 repo는 기존대로 unmanaged 보존). `GET /gc`
  dry-run은 읽기 전용: 스캔은 하되 시계를 기록하지 않는다.
- **보호 순서**: ① digest record 보유(digest-ref job/수동 digest pull — rule
  엔진 소유) ② in-use(이미지 ID·digest ref) ③ pin(`repo@digest`·이미지 ID —
  태그형 핀은 태그가 없으니 보호 불가) ④ 유예 미경과.
- **삭제 안무는 엔진 캡슐화**(`down.Reconciler.ReapUntagged`): 삭제 직전
  재-inspect(retag 시 skip), 멈춘 컨테이너 포함 참조 확인(All:true, 매 패스
  에러 스팸 방지), digest ref를 하나씩 제거(마지막 ref에서 content 해제,
  multi-repo by-ID force 불요) 후 ref 없는 이미지만 ID 삭제, NotFound=성공.
- **pull 경합**: job/수동 pull 공통 관문인 `dockerEngine.Pull`에 in-flight
  레지스트리를 두고, reap이 짧은 뮤텍스 안에서 확인+삭제한다. "Already
  exists" 직후 `ImageTag` 하려는 롤백 pull을 reap이 지워버리는 초 단위 경합이
  원천 차단된다(등록은 reap 진행 중에만 잠시 대기). 스캔 자체는 잠금 불요.
- **동일 데몬 이중 리퍼 금지**: 같은 address의 docker 스토어 둘이 모두 리퍼를
  켜면 config 검증에서 거부(서로의 pin/시계를 볼 수 없음).
- **관측 표면**: `Decision`에 `untagged`/`untagged_grace`/`digest_tracked`,
  `ApplyResult.reaped/skipped`, `gc_applied` detail의 `reaped`, `/v1/gc`의
  `untagged` 카운트·`untagged_after`, `/image`의 reap 시계 목록,
  `gantry.retention.untagged` 게이지.
