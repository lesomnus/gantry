<!-- Generated from internal/server/oapi/swagger.json by scripts/gen-api-docs.sh — do not edit by hand. -->

<!-- Generator: Widdershins v4.0.1 -->

<h1 id="gantry-api">gantry API v1.0</h1>

> Scroll down for code samples, example requests and responses. Select a language for code samples from the tabs above or the mobile navigation menu.

Move container images between stores (OCI registries and docker/containerd engines) and track per-layer progress.
A job copies an image `from` an OCI store `to` another, then `distribute` engines pull it.

# Authentication

* API Key (BearerAuth)
    - Parameter Name: **Authorization**, in: header. 

<h1 id="gantry-api-meta">meta</h1>

## Liveness probe

> Code samples

```shell
# You can also use wget
curl -X GET /healthz \
  -H 'Accept: text/plain'

```

`GET /healthz`

> Example responses

> 200 Response

```
"string"
```

<h3 id="liveness-probe-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|ok|string|

<aside class="success">
This operation does not require authentication
</aside>

<h1 id="gantry-api-jobs">jobs</h1>

## List jobs

> Code samples

```shell
# You can also use wget
curl -X GET /v1/job \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/job`

<h3 id="list-jobs-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|state|query|string|false|filter by state|
|ref|query|string|false|filter by ref substring|

> Example responses

> 200 Response

```json
{
  "items": [
    {
      "created_at": "string",
      "ended_at": "string",
      "error": "string",
      "id": "string",
      "platforms": [
        "string"
      ],
      "ref": "string",
      "started_at": "string",
      "state": "pending",
      "transfers": [
        {
          "bytes_done": 0,
          "bytes_total": 0,
          "error": "string",
          "from": "string",
          "kind": "string",
          "layers": [
            {
              "digest": "string",
              "done": 0,
              "platform": "string",
              "state": "string",
              "total": 0
            }
          ],
          "ref": "string",
          "state": "string",
          "store": "string"
        }
      ]
    }
  ]
}
```

<h3 id="list-jobs-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.jobListResponse](#schemaserver.joblistresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Create a job

> Code samples

```shell
# You can also use wget
curl -X POST /v1/job \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/job`

Move an image: copy `from` (oci) into `to` (oci), then have the `distribute` engines pull it. Idempotent per identical move.

> Body parameter

```json
{}
```

<h3 id="create-a-job-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|body|body|any|true|job request|

> Example responses

> 202 Response

```json
{
  "created_at": "string",
  "ended_at": "string",
  "error": "string",
  "id": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "started_at": "string",
  "state": "pending",
  "transfers": [
    {
      "bytes_done": 0,
      "bytes_total": 0,
      "error": "string",
      "from": "string",
      "kind": "string",
      "layers": [
        {
          "digest": "string",
          "done": 0,
          "platform": "string",
          "state": "string",
          "total": 0
        }
      ],
      "ref": "string",
      "state": "string",
      "store": "string"
    }
  ]
}
```

<h3 id="create-a-job-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|202|[Accepted](https://tools.ietf.org/html/rfc7231#section-6.3.3)|Accepted|[warm.JobSnapshot](#schemawarm.jobsnapshot)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|503|[Service Unavailable](https://tools.ietf.org/html/rfc7231#section-6.6.4)|Service Unavailable|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Cancel or evict a job

> Code samples

```shell
# You can also use wget
curl -X DELETE /v1/job/{id} \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`DELETE /v1/job/{id}`

<h3 id="cancel-or-evict-a-job-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|id|path|string|true|job id|

> Example responses

> 404 Response

```json
{
  "error": "string"
}
```

<h3 id="cancel-or-evict-a-job-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Get a job

> Code samples

```shell
# You can also use wget
curl -X GET /v1/job/{id} \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/job/{id}`

<h3 id="get-a-job-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|id|path|string|true|job id|

> Example responses

> 200 Response

```json
{
  "created_at": "string",
  "ended_at": "string",
  "error": "string",
  "id": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "started_at": "string",
  "state": "pending",
  "transfers": [
    {
      "bytes_done": 0,
      "bytes_total": 0,
      "error": "string",
      "from": "string",
      "kind": "string",
      "layers": [
        {
          "digest": "string",
          "done": 0,
          "platform": "string",
          "state": "string",
          "total": 0
        }
      ],
      "ref": "string",
      "state": "string",
      "store": "string"
    }
  ]
}
```

<h3 id="get-a-job-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[warm.JobSnapshot](#schemawarm.jobsnapshot)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Stream job progress

> Code samples

```shell
# You can also use wget
curl -X GET /v1/job/{id}/progress \
  -H 'Accept: text/event-stream' \
  -H 'Authorization: API_KEY'

```

`GET /v1/job/{id}/progress`

Server-Sent Events stream of progress; with ?wait=<dur> it long-polls and returns one JSON snapshot once the job is terminal.

<h3 id="stream-job-progress-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|id|path|string|true|job id|
|wait|query|string|false|long-poll duration, e.g. 30s|

> Example responses

> 200 Response

<h3 id="stream-job-progress-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[warm.JobSnapshot](#schemawarm.jobsnapshot)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

<h1 id="gantry-api-stores">stores</h1>

## List stores

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store`

Configured stores with their kind, capabilities, and readiness.

> Example responses

> 200 Response

```json
{
  "items": [
    {
      "address": "string",
      "capabilities": {
        "gc": true,
        "pull": true,
        "read": true,
        "verify": true,
        "write": true
      },
      "error": "string",
      "host": "string",
      "kind": "string",
      "mode": "string",
      "name": "string",
      "namespace": "string",
      "ready": true
    }
  ]
}
```

<h3 id="list-stores-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.storeListResponse](#schemaserver.storelistresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Trigger an engine store to pull

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/pull \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/store/{name}/pull`

Tells one engine store to pull a reference, decoupled from the job pipeline (manual reconcile).

> Body parameter

```json
{}
```

<h3 id="trigger-an-engine-store-to-pull-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|any|true|reference to pull|

> Example responses

> 200 Response

```json
{
  "kind": "string",
  "ref": "string",
  "state": "string",
  "store": "string"
}
```

<h3 id="trigger-an-engine-store-to-pull-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.storePullResponse](#schemaserver.storepullresponse)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

<h1 id="gantry-api-retention">retention</h1>

## Evaluate or run retention GC for a store

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/gc \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json'

```

`POST /v1/store/{name}/gc`

GET is a dry-run that returns the retention decision (keep/delete). POST applies the deletions and returns the apply result. An optional body overrides the configured max_age/keep_n/pins for this call.

> Body parameter

```json
{}
```

<h3 id="evaluate-or-run-retention-gc-for-a-store-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|any|false|policy overrides|

> Example responses

> 200 Response

```json
{
  "delete": [
    {
      "digest": "string",
      "last_used": "string",
      "reason": "string",
      "ref": "string"
    }
  ],
  "keep": [
    {
      "reason": "string",
      "ref": "string"
    }
  ]
}
```

<h3 id="evaluate-or-run-retention-gc-for-a-store-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|GET: dry-run decision; POST: apply result (retention.ApplyResult)|[retention.Decision](#schemaretention.decision)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="success">
This operation does not require authentication
</aside>

## Unpin a reference

> Code samples

```shell
# You can also use wget
curl -X DELETE /v1/store/{name}/pin \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json'

```

`DELETE /v1/store/{name}/pin`

> Body parameter

```json
{}
```

<h3 id="unpin-a-reference-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|any|true|reference to unpin|

> Example responses

> 400 Response

```json
{
  "error": "string"
}
```

<h3 id="unpin-a-reference-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="success">
This operation does not require authentication
</aside>

## List pinned references for a store

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/pin \
  -H 'Accept: application/json'

```

`GET /v1/store/{name}/pin`

Pinned references are exempt from retention GC (exact-match).

<h3 id="list-pinned-references-for-a-store-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|

> Example responses

> 200 Response

```json
{
  "pins": [
    "string"
  ]
}
```

<h3 id="list-pinned-references-for-a-store-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.pinListResponse](#schemaserver.pinlistresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="success">
This operation does not require authentication
</aside>

## Pin a reference (exempt from GC)

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/pin \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json'

```

`POST /v1/store/{name}/pin`

> Body parameter

```json
{}
```

<h3 id="pin-a-reference-(exempt-from-gc)-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|any|true|reference to pin|

> Example responses

> 400 Response

```json
{
  "error": "string"
}
```

<h3 id="pin-a-reference-(exempt-from-gc)-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="success">
This operation does not require authentication
</aside>

## Remove an image from an engine store

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/remove \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json'

```

`POST /v1/store/{name}/remove`

Manually deletes one image from an engine store and syncs the retention index.

> Body parameter

```json
{}
```

<h3 id="remove-an-image-from-an-engine-store-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|any|true|reference to remove|

> Example responses

> 200 Response

```json
{
  "deleted": [
    "string"
  ],
  "untagged": [
    "string"
  ]
}
```

<h3 id="remove-an-image-from-an-engine-store-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[down.RemoveResult](#schemadown.removeresult)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="success">
This operation does not require authentication
</aside>

# Schemas

<h2 id="tocS_down.RemoveResult">down.RemoveResult</h2>
<!-- backwards compatibility -->
<a id="schemadown.removeresult"></a>
<a id="schema_down.RemoveResult"></a>
<a id="tocSdown.removeresult"></a>
<a id="tocsdown.removeresult"></a>

```json
{
  "deleted": [
    "string"
  ],
  "untagged": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|deleted|[string]|false|none|none|
|untagged|[string]|false|none|none|

<h2 id="tocS_retention.Candidate">retention.Candidate</h2>
<!-- backwards compatibility -->
<a id="schemaretention.candidate"></a>
<a id="schema_retention.Candidate"></a>
<a id="tocSretention.candidate"></a>
<a id="tocsretention.candidate"></a>

```json
{
  "digest": "string",
  "last_used": "string",
  "reason": "string",
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|none|
|last_used|string|false|none|none|
|reason|string|false|none|age_exceeded|
|ref|string|false|none|none|

<h2 id="tocS_retention.Decision">retention.Decision</h2>
<!-- backwards compatibility -->
<a id="schemaretention.decision"></a>
<a id="schema_retention.Decision"></a>
<a id="tocSretention.decision"></a>
<a id="tocsretention.decision"></a>

```json
{
  "delete": [
    {
      "digest": "string",
      "last_used": "string",
      "reason": "string",
      "ref": "string"
    }
  ],
  "keep": [
    {
      "reason": "string",
      "ref": "string"
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|delete|[[retention.Candidate](#schemaretention.candidate)]|false|none|none|
|keep|[[retention.Kept](#schemaretention.kept)]|false|none|none|

<h2 id="tocS_retention.Kept">retention.Kept</h2>
<!-- backwards compatibility -->
<a id="schemaretention.kept"></a>
<a id="schema_retention.Kept"></a>
<a id="tocSretention.kept"></a>
<a id="tocsretention.kept"></a>

```json
{
  "reason": "string",
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|reason|string|false|none|in_use | pinned | keep_n_recent | within_max_age | grace | age_gc_disabled|
|ref|string|false|none|none|

<h2 id="tocS_server.createJobRequest">server.createJobRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.createjobrequest"></a>
<a id="schema_server.createJobRequest"></a>
<a id="tocSserver.createjobrequest"></a>
<a id="tocsserver.createjobrequest"></a>

```json
{
  "distribute": [
    "string"
  ],
  "from": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "to": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|distribute|[string]|false|none|none|
|from|string|false|none|none|
|platforms|[string]|false|none|none|
|ref|string|false|none|none|
|to|string|false|none|none|

<h2 id="tocS_server.errorResponse">server.errorResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.errorresponse"></a>
<a id="schema_server.errorResponse"></a>
<a id="tocSserver.errorresponse"></a>
<a id="tocsserver.errorresponse"></a>

```json
{
  "error": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|error|string|false|none|none|

<h2 id="tocS_server.gcRequest">server.gcRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.gcrequest"></a>
<a id="schema_server.gcRequest"></a>
<a id="tocSserver.gcrequest"></a>
<a id="tocsserver.gcrequest"></a>

```json
{
  "keep_n": 0,
  "max_age": "720h",
  "pins": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|keep_n|integer|false|none|none|
|max_age|string|false|none|none|
|pins|[string]|false|none|none|

<h2 id="tocS_server.jobListResponse">server.jobListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.joblistresponse"></a>
<a id="schema_server.jobListResponse"></a>
<a id="tocSserver.joblistresponse"></a>
<a id="tocsserver.joblistresponse"></a>

```json
{
  "items": [
    {
      "created_at": "string",
      "ended_at": "string",
      "error": "string",
      "id": "string",
      "platforms": [
        "string"
      ],
      "ref": "string",
      "started_at": "string",
      "state": "pending",
      "transfers": [
        {
          "bytes_done": 0,
          "bytes_total": 0,
          "error": "string",
          "from": "string",
          "kind": "string",
          "layers": [
            {
              "digest": "string",
              "done": 0,
              "platform": "string",
              "state": "string",
              "total": 0
            }
          ],
          "ref": "string",
          "state": "string",
          "store": "string"
        }
      ]
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|items|[[warm.JobSnapshot](#schemawarm.jobsnapshot)]|false|none|none|

<h2 id="tocS_server.pinListResponse">server.pinListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.pinlistresponse"></a>
<a id="schema_server.pinListResponse"></a>
<a id="tocSserver.pinlistresponse"></a>
<a id="tocsserver.pinlistresponse"></a>

```json
{
  "pins": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|pins|[string]|false|none|none|

<h2 id="tocS_server.pinRequest">server.pinRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.pinrequest"></a>
<a id="schema_server.pinRequest"></a>
<a id="tocSserver.pinrequest"></a>
<a id="tocsserver.pinrequest"></a>

```json
{
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|ref|string|false|none|none|

<h2 id="tocS_server.removeRequest">server.removeRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.removerequest"></a>
<a id="schema_server.removeRequest"></a>
<a id="tocSserver.removerequest"></a>
<a id="tocsserver.removerequest"></a>

```json
{
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|ref|string|false|none|none|

<h2 id="tocS_server.storeListResponse">server.storeListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.storelistresponse"></a>
<a id="schema_server.storeListResponse"></a>
<a id="tocSserver.storelistresponse"></a>
<a id="tocsserver.storelistresponse"></a>

```json
{
  "items": [
    {
      "address": "string",
      "capabilities": {
        "gc": true,
        "pull": true,
        "read": true,
        "verify": true,
        "write": true
      },
      "error": "string",
      "host": "string",
      "kind": "string",
      "mode": "string",
      "name": "string",
      "namespace": "string",
      "ready": true
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|items|[[store.Status](#schemastore.status)]|false|none|none|

<h2 id="tocS_server.storePullRequest">server.storePullRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.storepullrequest"></a>
<a id="schema_server.storePullRequest"></a>
<a id="tocSserver.storepullrequest"></a>
<a id="tocsserver.storepullrequest"></a>

```json
{
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|ref|string|false|none|none|

<h2 id="tocS_server.storePullResponse">server.storePullResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.storepullresponse"></a>
<a id="schema_server.storePullResponse"></a>
<a id="tocSserver.storepullresponse"></a>
<a id="tocsserver.storepullresponse"></a>

```json
{
  "kind": "string",
  "ref": "string",
  "state": "string",
  "store": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|kind|string|false|none|none|
|ref|string|false|none|none|
|state|string|false|none|none|
|store|string|false|none|none|

<h2 id="tocS_store.Caps">store.Caps</h2>
<!-- backwards compatibility -->
<a id="schemastore.caps"></a>
<a id="schema_store.Caps"></a>
<a id="tocSstore.caps"></a>
<a id="tocsstore.caps"></a>

```json
{
  "gc": true,
  "pull": true,
  "read": true,
  "verify": true,
  "write": true
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|gc|boolean|false|none|none|
|pull|boolean|false|none|none|
|read|boolean|false|none|none|
|verify|boolean|false|none|none|
|write|boolean|false|none|none|

<h2 id="tocS_store.Status">store.Status</h2>
<!-- backwards compatibility -->
<a id="schemastore.status"></a>
<a id="schema_store.Status"></a>
<a id="tocSstore.status"></a>
<a id="tocsstore.status"></a>

```json
{
  "address": "string",
  "capabilities": {
    "gc": true,
    "pull": true,
    "read": true,
    "verify": true,
    "write": true
  },
  "error": "string",
  "host": "string",
  "kind": "string",
  "mode": "string",
  "name": "string",
  "namespace": "string",
  "ready": true
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|address|string|false|none|none|
|capabilities|[store.Caps](#schemastore.caps)|false|none|none|
|error|string|false|none|none|
|host|string|false|none|none|
|kind|string|false|none|none|
|mode|string|false|none|none|
|name|string|false|none|none|
|namespace|string|false|none|none|
|ready|boolean|false|none|none|

<h2 id="tocS_warm.JobSnapshot">warm.JobSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.jobsnapshot"></a>
<a id="schema_warm.JobSnapshot"></a>
<a id="tocSwarm.jobsnapshot"></a>
<a id="tocswarm.jobsnapshot"></a>

```json
{
  "created_at": "string",
  "ended_at": "string",
  "error": "string",
  "id": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "started_at": "string",
  "state": "pending",
  "transfers": [
    {
      "bytes_done": 0,
      "bytes_total": 0,
      "error": "string",
      "from": "string",
      "kind": "string",
      "layers": [
        {
          "digest": "string",
          "done": 0,
          "platform": "string",
          "state": "string",
          "total": 0
        }
      ],
      "ref": "string",
      "state": "string",
      "store": "string"
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|created_at|string|false|none|none|
|ended_at|string|false|none|none|
|error|string|false|none|none|
|id|string|false|none|none|
|platforms|[string]|false|none|none|
|ref|string|false|none|none|
|started_at|string|false|none|none|
|state|[warm.JobState](#schemawarm.jobstate)|false|none|none|
|transfers|[[warm.TransferSnapshot](#schemawarm.transfersnapshot)]|false|none|none|

<h2 id="tocS_warm.JobState">warm.JobState</h2>
<!-- backwards compatibility -->
<a id="schemawarm.jobstate"></a>
<a id="schema_warm.JobState"></a>
<a id="tocSwarm.jobstate"></a>
<a id="tocswarm.jobstate"></a>

```json
"pending"

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|*anonymous*|string|false|none|none|

#### Enumerated Values

|Property|Value|
|---|---|
|*anonymous*|pending|
|*anonymous*|running|
|*anonymous*|done|
|*anonymous*|failed|
|*anonymous*|canceled|

<h2 id="tocS_warm.LayerSnapshot">warm.LayerSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.layersnapshot"></a>
<a id="schema_warm.LayerSnapshot"></a>
<a id="tocSwarm.layersnapshot"></a>
<a id="tocswarm.layersnapshot"></a>

```json
{
  "digest": "string",
  "done": 0,
  "platform": "string",
  "state": "string",
  "total": 0
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|none|
|done|integer|false|none|none|
|platform|string|false|none|none|
|state|string|false|none|none|
|total|integer|false|none|none|

<h2 id="tocS_warm.TransferSnapshot">warm.TransferSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.transfersnapshot"></a>
<a id="schema_warm.TransferSnapshot"></a>
<a id="tocSwarm.transfersnapshot"></a>
<a id="tocswarm.transfersnapshot"></a>

```json
{
  "bytes_done": 0,
  "bytes_total": 0,
  "error": "string",
  "from": "string",
  "kind": "string",
  "layers": [
    {
      "digest": "string",
      "done": 0,
      "platform": "string",
      "state": "string",
      "total": 0
    }
  ],
  "ref": "string",
  "state": "string",
  "store": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|bytes_done|integer|false|none|none|
|bytes_total|integer|false|none|none|
|error|string|false|none|none|
|from|string|false|none|none|
|kind|string|false|none|none|
|layers|[[warm.LayerSnapshot](#schemawarm.layersnapshot)]|false|none|none|
|ref|string|false|none|none|
|state|string|false|none|none|
|store|string|false|none|none|

