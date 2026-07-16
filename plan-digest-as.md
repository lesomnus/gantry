# digest-pinned 워밍: `as`의 digest reference 지원

jobspec이 digest로 핀된 경우(`image = repo@sha256:INDEX`) 노드가 그 **원본 이름 그대로**
로컬 resolve 하도록, 워밍이 노드 엔진에 digest-named reference를 만들어 준다.
`docker image inspect repo@sha256:INDEX`가 네트워크 없이 성공하고, `force_pull=false`
컨테이너가 pull 없이 뜬다 — 완전 skip.

## 문제 (현상 → 근본 원인)

- `as`는 태그 전용이다(`warm.go` plan: digest `as`는 admission에서 거부). 그래서
  digest-pinned 잡을 워밍해도 노드에는 `cachehost/repo@sha256:INDEX`(pull이 만든
  기록)만 남고 **원본 이름 `repo@sha256:INDEX`에 대응하는 로컬 참조가 없다**.
- 노드 docker의 `ImageInspect("repo@sha256:INDEX")` miss → docker가 이름 속
  레지스트리(원본)로 manifest를 재조회한다. 레이어는 로컬이라 바이트는 안 움직여도
  네트워크 왕복이 남는다.
- classic(graph) store에서는 RepoDigest가 실제 pull로만 생기므로 이 참조를 위조할
  방법 자체가 없었다 — 태그 전용이었던 이유.
- **doc/impl 갭 판정**: proto 주석은 "Tag references only"라고 정확히 적혀 있다
  (과장 없음). README의 "engine pulls are digest-anchored"는 *pull 앵커링*(repo@digest로
  당긴다) 이야기지 digest-named 로컬 참조가 남는다는 뜻이 아니다 — 오독의 여지가
  있어 문구를 명확화한다. 오히려 기존 설계는 digest-named 레코드를 **의도적으로
  삭제**했다(containerd `as` 경로, retention이 태그만 추적하므로).

## 환경 전제와 PoC 결과 (192.168.9.0/24 실노드 검증, 2026-07-16)

대상 노드(docker 29.6.1, **containerd image store** — `driver-type:
io.containerd.snapshotter.v1`)에서 다음을 실증했다:

| 검증 | 결과 |
| --- | --- |
| `ctr -n moby images tag cache@D origin@D` — out-of-band digest 참조가 docker에 보이는가 | ✅ inspect OK, RepoDigests에 포함 |
| **씬 OCI 아카이브 `docker load`** — `oci-layout` + `index.json`(엔트리 annotation `io.containerd.image.name=origin/repo@D`) + index blob 하나(~20KB) | ✅ digest 이름 등록, inspect/run OK |
| `origin.invalid`(resolve 불가 도메인)로 이름 지어 네트워크 의존 배제 증명 | ✅ 접속 시도 0회 (daemon 로그) |
| 멀티아치: index digest → 노드 arch manifest resolve | ✅ amd64 |
| 재로드 멱등성 / `rmi`는 해당 이름만 제거(콘텐츠·캐시 이름 보존) | ✅ |
| **콘텐츠가 없는 상태로 load** | ⚠️ 이름은 등록되지만 깨진 레코드 — run이 원본 레지스트리로 fallback. **이름 등록은 반드시 pull 성공 후** |

핵심: 씬 아카이브 `docker load`는 **순수 Engine API**다. gantry가 원격 엔진에
가진 유일한 채널(tcp 2375)로 충분하고, containerd 소켓 접근이 필요 없다.

## 설계 결정

### 1. API: `as`를 digest reference로 확장 (신규 필드 없음)

- proto는 `repeated string as` 그대로 — 구조 변경 없음, 주석만 갱신. 기존에
  거부되던 입력을 받아들이는 완화라 하위호환이다. 태그 `as`의 의미·동작 불변.
- **제약** (admission에서 검증):
  - digest `as`는 **앵커된 pull에서만** 허용 — 잡 ref가 digest ref거나, verify가
    digest를 핀한 경우. 앵커가 없으면 "무엇의 이름인지"가 정의되지 않는다.
  - `as` 이름에 박힌 digest는 **앵커 digest와 일치**해야 한다. 불일치는 거짓 참조
    (콘텐츠가 그 digest가 아님)이므로 거부.
- 대안으로 검토한 신규 필드(`record_digests`)는 기각: `as`의 의미("pull한 이미지를
  이 이름들로 기록")가 digest 이름에도 정확히 들어맞고, dedup·Plan echo·retention
  스탬프 등 기존 배선을 전부 재사용한다.

### 2. 참조 생성 메커니즘: 엔진 종류별

- **containerd 엔진**: `ImageService().Create(images.Image{Name: <digest-name>,
  Target: img.Target()})` — 기존 태그 `as`와 같은 프리미티브. 앵커된 pull이므로
  `img.Target().Digest == 앵커`를 확인 후 기록. 콘텐츠 쓰기 불필요(pull이 index까지
  이미 적재).
- **docker 엔진 (containerd image store)**: warm이 전달한 **원본 index/manifest
  바이트**로 씬 OCI 아카이브를 만들어 `ImageLoad`(POST /images/load) — PoC 방식
  그대로. 아카이브 = `oci-layout` + `index.json`(digest 이름마다 엔트리, annotation
  `io.containerd.image.name`) + 앵커 blob 하나. KB 단위.
- **docker 엔진 (classic graph store)**: 감지 시 **경고 로그 + digest 이름 skip**
  (best-effort no-op). 태그 이름은 정상 처리, 잡은 성공. classic에선 위조가 원천
  불가능하므로 이것이 상한이다.
  - 감지: `Info().DriverStatus`에 `driver-type: io.containerd.snapshotter.v1`
    존재 여부. 최초 1회 캐시(플랫폼 프로브와 동일 패턴).

### 3. 바이트 출처: 캐시만 (two-hop 유지)

- 앵커 blob(index/manifest 바이트)은 hop2 잡의 **source(=캐시)**에서
  `remote.Get`(digest ref — ggcr이 digest 일치 검증)으로 가져온다. 기존 per-store
  transport(insecure/mTLS/TPM) 그대로. **원 레지스트리 접속 없음.**
- 레이어·컨피그는 기존대로 데몬이 캐시에서 pull.
- 순서 불변식(PoC): **pull 성공 → 그 다음에 이름 등록.** 등록 실패 시 기존 `as`
  실패와 동일한 정리 규칙(아직 아무 이름도 못 붙였고 앵커된 pull이면 pull 레코드
  best-effort 제거) 후 잡 실패.

### 4. hop1 회귀 수정: digest-ref 레지스트리 잡은 verbatim 커밋

- rewrite는 digest를 보존하므로 digest-ref 잡의 cacheRef는 `cache/repo@D`다. 그런데
  기존 `Commit`은 verbatim이 아니면 index를 **재조립**(자식 필터·annotation 유실 →
  digest 변경)해서 digest URL PUT이 레지스트리에서 거부된다. → **dst가 digest ref면
  verbatim 커밋을 강제**해 원본 index 바이트(=D)를 보존한다.
- digest-ref 잡 + `platforms` 좁히기는 모순(좁히면 D를 보존할 수 없음)이므로
  admission에서 거부 — `copy_referrers`의 기존 규칙과 동일한 형태.

### 5. GC / retention 정합

- pullHook은 지금도 `as` 이름을 그대로 스탬프한다 → digest 이름도 retention index에
  기록된다(`parseRef`가 digest ref를 이미 처리). rule GC가 이름으로 `Remove` 가능,
  docker untagged reaper의 `digest_tracked` 보호와도 정합.
- containerd의 "digest-named 레코드는 남기지 않는다" 불변식은 "**요청되지 않은**
  digest 레코드는 남기지 않는다"로 조건이 완화된다(주석·통합 테스트 갱신).
  요청된 digest 이름은 index가 추적하므로 콘텐츠를 영원히 root하지 않는다.
- referrers: 노드 쪽 digest 참조는 referrers가 필요 없다(노드에서 서명 검증하는
  경로 없음). 서명은 캐시에 같은 INDEX digest로 남아 있다(copy_referrers).

## 적대적 리뷰에서 나온 수정 (2026-07-16)

멀티 에이전트 리뷰(4개 차원 → 발견별 반박 검증)에서 확정된 이슈와 반영:

1. **(major) classic-store skip이 retention index를 오염** — 엔진이 digest 이름을
   조용히 skip해도 warm이 요청된 이름을 그대로 스탬프 → 데몬엔 없는 이름이 index에
   기록되고, 실제 이미지는 untagged로 분류돼 `untagged_after` 후 **자기가 배치한
   이미지를 gantry가 삭제**. → `Engine.Pull`이 **실제 기록된 이름(recorded)을 반환**
   하고 warm은 그것만 스탬프(비면 pullRef 폴백 — 종전 동작과 동일).
2. **(major) plan/apply 레이스로 갓 로드된 digest 이름을 reaper가 삭제** — 로드된
   digest 참조는 태그 재확인으로 보호되지 않음. → `ReapUntagged`에 **owned predicate**
   추가: apply 시점에 라이브 index의 digest 레코드를 재조회해 소유 참조가 있으면 거부.
3. **(minor) 이미지 스토어 프로브 영구 캐시** — 데몬이 classic↔containerd로 마이그레이션
   하면 재시작 전까지 오판. → 캐시 제거, digest-`as` pull마다 프로브(Info 1회, 저비용).
4. **(minor) proxy 캐시 회귀** — digest ref + platforms 조합을 proxy 모드에서도 거부했음.
   proxy는 커밋이 없으므로 제약 불필요. → copy 모드에만 적용.
5. **(minor) 앵커 바이트 신뢰** — ggcr가 일부 legacy media type에서 digest 검증을 생략할
   수 있음. → `fetchAnchor`가 sha256을 직접 재계산·비교(비 sha256 digest는 거부).

## 산출물 체크리스트

- [x] PoC (실노드, 위 표)
- [x] 구현: `internal/warm`(plan 검증·앵커 fetch·verbatim), `internal/down`(docker
  load 경로·containerd digest 레코드·store 감지), proto 주석
- [x] 테스트: admission 규칙, docker 씬 로드(fake daemon), containerd digest 레코드
  (사이드카 live), verbatim 커밋, 태그 `as` 회귀 — `go test -race ./...` green
- [x] E2E (실노드, 2026-07-16): 수용 기준 4종 모두 통과
  - hop1 digest-ref → 캐시 verbatim 커밋 (커밋 digest == INDEX, 바이트 sha 일치)
  - **origin 레지스트리 정지 상태에서** hop2 완료; thorb에서
    `docker image inspect <origin>/e2e/app@sha256:INDEX` 성공, RepoDigests에
    요청한 digest 이름 2개(실호스트 + `.invalid` 호스트), daemon 로그에 origin
    접속 0회
  - `docker run --pull=missing <origin>@INDEX` 즉시 실행 (pull 없음)
  - 멀티아치: 노드 content store에 index 블롭 + amd64 manifest, 등록된 이름의
    target mediaType == `application/vnd.oci.image.index.v1+json`, arch 정상 resolve
  - 태그 `as`/plain 워밍 회귀 없음 (advantech)
  - 라이프사이클: digest 이름 하나만 `StoreService.Remove` → 그 이름만 untag,
    나머지 이름·콘텐츠 보존
- [x] 문서: README `as` 절 추가·status 문구 명확화, docs/test-environment.md,
  proto 주석(job.proto + pb 미러; 신규 generator 드리프트 때문에 전체 regen은
  보류 — 주석만 수동 동기화)
