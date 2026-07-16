  ## 문제
  jobspec이 **digest로 핀**된 경우 — image = "repo@sha256:INDEX" (보통 OCI image index /
  manifest list 의 digest) — 완전 skip이 안 된다.
  - 현재 as는 사실상 태그 retag만 한다. digest ref에 대응하는 로컬 이름(RepoDigest)이 안 생긴다.
  - 그래서 노드 docker 드라이버의 ImageInspect("repo@sha256:INDEX") 가 miss → docker가
    **원 레지스트리로 manifest를 재조회**한다(레이어는 로컬이라 다운로드는 없지만 네트워크
    왕복이 남는다). 즉 완전 skip이 아니다.
  - 근본 원인: classic docker(graph) store에선 retag이 RepoTag만 만들고, RepoDigest는 실제
    pull로만 생겨서 위조가 불가능하다.
  - (참고: gantry 문서에는 as가 "digest-anchored"라고 적혀 있을 수 있는데, 실제 구현은 태그
    전용으로 보인다. 이 doc/impl 갭도 함께 확인·정리해달라.)

  ## 환경 (중요 — 이게 해결의 열쇠)
  대상 노드의 docker는 **containerd image store**를 사용한다.
  → 모든 것이 content-addressed 블롭(index, platform manifest, config, layer)으로 저장되고,
    **이미 로컬에 있는 콘텐츠에 원본 이름의 digest 참조를 만들 수 있다.** classic store의
    "RepoDigest 위조 불가" 제약이 사라진다.

  ## 필요한 기능
  digest-pinned 이미지를 hop2로 워밍할 때, 노드 containerd content store에
  **index + 그 노드 플랫폼의 manifest + config + layers**를 배치하고,
  **원본 이름 `repo@sha256:INDEX` 로 image 참조를 등록**해서
  `docker image inspect repo@sha256:INDEX` 가 **네트워크 없이 로컬로 resolve** 되게 한다.

  제약:
  - 바이트는 cache 스토어에서 온다(two-hop 유지). **원 레지스트리에 접속하지 않고** 원본-이름
    참조를 로컬에서 만들어야 한다.
  - 태그 ref의 기존 동작은 회귀 없이 그대로.
  - classic(graph) store에선 우아하게 best-effort/no-op 폴백(명확한 경고 로그).

  ## 먼저 조사할 것
  1. 현재 as / hop2가 engine(docker) 스토어에 어떻게 기록하는지 — docker Engine API(retag/load)
     인지 containerd API 직접인지.
  2. containerd `moby` 네임스페이스에 out-of-band로 만든 image가 docker Engine API의
     ImageInspect / `docker inspect` 에 그대로 보이는지 (← 이 방식 성립의 핵심 검증).
  3. index(manifest list) vs 단일 platform manifest 처리, copy_referrers 와의 상호작용.
  4. gRPC API를 바꿔야 하는지: as 가 digest ref를 받도록 확장할지, 신규 필드를 둘지. 하위호환 유지.

  ## 수용 기준 (실제 containerd-store docker로 end-to-end 검증)
  1. 워밍 후 노드에서 `docker image inspect <repo>@sha256:<INDEX>` 성공, RepoDigests에
     `<repo>@sha256:<INDEX>` 포함, 이 과정에서 원 레지스트리 접속 없음(캐시만 사용).
  2. image=<repo>@sha256:<INDEX>, force_pull=false 인 컨테이너 실행이 pull 없이 뜬다
     (docker 데몬 로그 / registry 미접속으로 확인).
  3. 멀티아치: 노드 arch의 manifest가 로컬에 있고 index digest로 resolve된다.
  4. 기존 태그 기반 as 워밍 회귀 없음.

  ## 산출물
  - 설계 제안(참조 생성을 docker API로 할지 containerd API로 할지, `moby` 네임스페이스 가시성
    검증 결과 포함), 구현, 테스트, 문서 갱신.
  - proto/gRPC 변경이 필요하면 포함하되 하위호환 유지.
  - 먼저 192.168.9.0/24에서 재현(캐시에서 받은 콘텐츠로 repo@INDEX 참조를 만들고 docker가
    로컬 hit 하는지)해 방식이 성립하는지 확인한 뒤 구현에 들어갈 것.
