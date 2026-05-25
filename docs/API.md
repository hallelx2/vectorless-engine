# Vectorless Engine API Reference

Base URL: `http://localhost:8080`

All endpoints are prefixed with `/v1`. Request and response bodies use JSON unless otherwise noted.

---

## Table of Contents

- [Health Check](#health-check)
- [Version](#version)
- [Documents](#documents)
  - [Ingest Document](#ingest-document)
  - [List Documents](#list-documents)
  - [Get Document](#get-document)
  - [Delete Document](#delete-document)
  - [Get Document Tree](#get-document-tree)
  - [Get Document Source](#get-document-source)
- [Sections](#sections)
  - [Get Section](#get-section)
- [Query](#query)
  - [Single-Document Query](#single-document-query)
  - [Multi-Document Query](#multi-document-query)

---

## Health Check

Check service health and readiness.

**Endpoint**

```
GET /v1/health
```

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Service is healthy |
| `503 Service Unavailable` | Service is not ready (database unreachable, etc.) |

```json
{
  "status": "ok",
  "database": "connected",
  "uptime": "2h15m30s"
}
```

**Example**

```bash
curl http://localhost:8080/v1/health
```

---

## Version

Return the engine version and build information.

**Endpoint**

```
GET /v1/version
```

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Version info returned |

```json
{
  "version": "0.1.0",
  "go_version": "go1.25",
  "build_time": "2026-04-20T10:00:00Z",
  "commit": "abc1234"
}
```

**Example**

```bash
curl http://localhost:8080/v1/version
```

---

## Documents

### Ingest Document

Upload and process a document. Supports multipart file upload or JSON body with raw content.

**Endpoint**

```
POST /v1/documents/
```

**Option A: Multipart Upload**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `file` | file | Yes | The document file (PDF, DOCX, TXT, MD, HTML) |
| `title` | string | No | Human-readable title. Defaults to the filename. |
| `metadata` | string (JSON) | No | Arbitrary JSON metadata to attach to the document. |

```bash
curl -X POST http://localhost:8080/v1/documents/ \
  -F "file=@report.pdf" \
  -F 'title=Q1 Report' \
  -F 'metadata={"department":"finance"}'
```

**Option B: JSON Body**

```
Content-Type: application/json
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `content` | string | Yes | Raw text content of the document. |
| `title` | string | Yes | Document title. |
| `content_type` | string | No | MIME type (e.g., `text/plain`, `text/markdown`). Defaults to `text/plain`. |
| `metadata` | object | No | Arbitrary metadata. |

```bash
curl -X POST http://localhost:8080/v1/documents/ \
  -H "Content-Type: application/json" \
  -d '{
    "content": "This is the full text of my document...",
    "title": "My Document",
    "content_type": "text/markdown",
    "metadata": {"author": "Jane Doe"}
  }'
```

**Response**

| Status | Description |
|--------|-------------|
| `201 Created` | Document ingested and processing started |
| `400 Bad Request` | Invalid input (missing file/content, unsupported format) |
| `413 Payload Too Large` | File exceeds size limit |
| `500 Internal Server Error` | Processing error |

```json
{
  "id": "doc_abc123",
  "title": "Q1 Report",
  "content_type": "application/pdf",
  "status": "processing",
  "section_count": 0,
  "metadata": {
    "department": "finance"
  },
  "created_at": "2026-04-20T10:30:00Z",
  "updated_at": "2026-04-20T10:30:00Z"
}
```

---

### List Documents

Retrieve a paginated list of documents.

**Endpoint**

```
GET /v1/documents/
```

**Query Parameters**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `page` | integer | `1` | Page number (1-indexed) |
| `page_size` | integer | `20` | Number of results per page (max 100) |
| `status` | string | -- | Filter by status: `processing`, `ready`, `failed` |
| `q` | string | -- | Search documents by title (case-insensitive substring match) |

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | List returned |
| `400 Bad Request` | Invalid pagination parameters |

```json
{
  "documents": [
    {
      "id": "doc_abc123",
      "title": "Q1 Report",
      "content_type": "application/pdf",
      "status": "ready",
      "section_count": 14,
      "metadata": {
        "department": "finance"
      },
      "created_at": "2026-04-20T10:30:00Z",
      "updated_at": "2026-04-20T10:31:00Z"
    }
  ],
  "total": 42,
  "page": 1,
  "page_size": 20
}
```

**Example**

```bash
curl "http://localhost:8080/v1/documents/?page=1&page_size=10&status=ready"
```

---

### Get Document

Retrieve a single document by ID.

**Endpoint**

```
GET /v1/documents/{id}
```

**Path Parameters**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | string | Document ID |

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Document returned |
| `404 Not Found` | Document does not exist |

```json
{
  "id": "doc_abc123",
  "title": "Q1 Report",
  "content_type": "application/pdf",
  "status": "ready",
  "section_count": 14,
  "metadata": {
    "department": "finance"
  },
  "created_at": "2026-04-20T10:30:00Z",
  "updated_at": "2026-04-20T10:31:00Z"
}
```

**Example**

```bash
curl http://localhost:8080/v1/documents/doc_abc123
```

---

### Delete Document

Delete a document and all of its associated sections and embeddings.

**Endpoint**

```
DELETE /v1/documents/{id}
```

**Path Parameters**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | string | Document ID |

**Response**

| Status | Description |
|--------|-------------|
| `204 No Content` | Document deleted |
| `404 Not Found` | Document does not exist |

**Example**

```bash
curl -X DELETE http://localhost:8080/v1/documents/doc_abc123
```

---

### Get Document Tree

Retrieve the hierarchical section tree of a document. Useful for displaying a table of contents or navigating the document structure.

**Endpoint**

```
GET /v1/documents/{id}/tree
```

**Path Parameters**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | string | Document ID |

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Tree returned |
| `404 Not Found` | Document does not exist |

```json
{
  "document_id": "doc_abc123",
  "tree": [
    {
      "id": "sec_001",
      "title": "Introduction",
      "level": 1,
      "children": [
        {
          "id": "sec_002",
          "title": "Background",
          "level": 2,
          "children": []
        },
        {
          "id": "sec_003",
          "title": "Objectives",
          "level": 2,
          "children": []
        }
      ]
    },
    {
      "id": "sec_004",
      "title": "Methodology",
      "level": 1,
      "children": []
    }
  ]
}
```

**Example**

```bash
curl http://localhost:8080/v1/documents/doc_abc123/tree
```

---

### Get Document Source

Download the original uploaded document bytes (the raw file).

**Endpoint**

```
GET /v1/documents/{id}/source
```

**Path Parameters**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | string | Document ID |

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Raw file bytes with appropriate `Content-Type` and `Content-Disposition` headers |
| `404 Not Found` | Document does not exist |
| `410 Gone` | Source file is no longer available (e.g., retention policy) |

The response body is the raw binary content of the original file. The `Content-Type` header matches the original upload type (e.g., `application/pdf`).

**Example**

```bash
curl -o report.pdf http://localhost:8080/v1/documents/doc_abc123/source
```

---

## Sections

### Get Section

Retrieve a single section by ID, including its text content.

**Endpoint**

```
GET /v1/sections/{id}
```

**Path Parameters**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | string | Section ID |

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Section returned |
| `404 Not Found` | Section does not exist |

```json
{
  "id": "sec_001",
  "document_id": "doc_abc123",
  "title": "Introduction",
  "level": 1,
  "content": "This report covers the financial performance of...",
  "token_count": 312,
  "position": 0,
  "created_at": "2026-04-20T10:30:45Z"
}
```

**Example**

```bash
curl http://localhost:8080/v1/sections/sec_001
```

---

## Query

### Single-Document Query

Perform a semantic query against a single document. The engine retrieves the most relevant sections and generates an answer using the LLM.

**Endpoint**

```
POST /v1/query
```

**Request Body**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `document_id` | string | Yes | ID of the document to query |
| `query` | string | Yes | Natural-language question or search query |
| `top_k` | integer | No | Number of sections to retrieve (default: `5`, max: `20`) |
| `include_sources` | boolean | No | Whether to include source section IDs in the response (default: `true`) |

```json
{
  "document_id": "doc_abc123",
  "query": "What was the revenue in Q1?",
  "top_k": 5,
  "include_sources": true
}
```

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Query results returned |
| `400 Bad Request` | Missing or invalid fields |
| `404 Not Found` | Document does not exist |
| `500 Internal Server Error` | Query processing error |

```json
{
  "answer": "The Q1 revenue was $4.2M, representing a 15% increase over the previous quarter.",
  "sources": [
    {
      "section_id": "sec_007",
      "title": "Financial Summary",
      "score": 0.94,
      "content_preview": "Total revenue for the first quarter reached $4.2 million..."
    },
    {
      "section_id": "sec_008",
      "title": "Quarter-over-Quarter Comparison",
      "score": 0.87,
      "content_preview": "Compared to Q4, this represents a 15% increase driven by..."
    }
  ],
  "model": "claude-sonnet-4-20250514",
  "usage": {
    "prompt_tokens": 2048,
    "completion_tokens": 150
  }
}
```

**Example**

```bash
curl -X POST http://localhost:8080/v1/query \
  -H "Content-Type: application/json" \
  -d '{
    "document_id": "doc_abc123",
    "query": "What was the revenue in Q1?",
    "top_k": 5
  }'
```

---

### Multi-Document Query

Perform a semantic query across multiple documents. Results are merged and ranked by relevance.

**Endpoint**

```
POST /v1/query/multi
```

**Request Body**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `document_ids` | string[] | Yes | List of document IDs to query across |
| `query` | string | Yes | Natural-language question or search query |
| `top_k` | integer | No | Number of sections to retrieve per document (default: `3`, max: `10`) |
| `include_sources` | boolean | No | Whether to include source section IDs in the response (default: `true`) |

```json
{
  "document_ids": ["doc_abc123", "doc_def456", "doc_ghi789"],
  "query": "Compare the revenue trends across all quarters",
  "top_k": 3,
  "include_sources": true
}
```

**Response**

| Status | Description |
|--------|-------------|
| `200 OK` | Query results returned |
| `400 Bad Request` | Missing or invalid fields (empty document list, etc.) |
| `404 Not Found` | One or more documents do not exist |
| `500 Internal Server Error` | Query processing error |

```json
{
  "answer": "Across the three reports, revenue grew consistently: Q1 saw $4.2M, Q2 reached $4.8M, and Q3 hit $5.1M...",
  "sources": [
    {
      "document_id": "doc_abc123",
      "section_id": "sec_007",
      "title": "Financial Summary",
      "score": 0.94,
      "content_preview": "Total revenue for the first quarter reached $4.2 million..."
    },
    {
      "document_id": "doc_def456",
      "section_id": "sec_012",
      "title": "Q2 Revenue Overview",
      "score": 0.91,
      "content_preview": "Second quarter revenue totaled $4.8 million, a 14% increase..."
    },
    {
      "document_id": "doc_ghi789",
      "section_id": "sec_003",
      "title": "Q3 Highlights",
      "score": 0.89,
      "content_preview": "Revenue in Q3 reached $5.1 million, continuing the upward trend..."
    }
  ],
  "model": "claude-sonnet-4-20250514",
  "usage": {
    "prompt_tokens": 4096,
    "completion_tokens": 250
  }
}
```

**Example**

```bash
curl -X POST http://localhost:8080/v1/query/multi \
  -H "Content-Type: application/json" \
  -d '{
    "document_ids": ["doc_abc123", "doc_def456"],
    "query": "Compare the revenue trends across all quarters",
    "top_k": 3
  }'
```

---

## Common Error Response Format

All error responses follow a consistent structure:

```json
{
  "error": {
    "code": "not_found",
    "message": "Document with ID 'doc_xyz' was not found"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `error.code` | string | Machine-readable error code |
| `error.message` | string | Human-readable error description |

### Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `bad_request` | 400 | Invalid or missing request parameters |
| `not_found` | 404 | Resource does not exist |
| `payload_too_large` | 413 | Uploaded file exceeds size limit |
| `internal_error` | 500 | Unexpected server error |
| `service_unavailable` | 503 | Service is not ready |
