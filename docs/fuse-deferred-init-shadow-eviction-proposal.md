# Proposal: shadow-backed part eviction for unknown-size FUSE uploads

**Date**: 2026-04-16  
**Purpose**: Define a follow-up enhancement for FUSE create and truncate write paths so they can keep bounded memory usage without issuing an invalid multipart initiate before the final file size is known.

## Summary

`drive9` should keep the fix that prevents unknown-size FUSE create and truncate handles from initiating multipart uploads during `Write()`, but it should not accept the resulting O(file size) memory growth as the long-term behavior.

The follow-up design is:

- keep remote multipart initiation deferred until `Flush()` or `Release()`, when the final file size is known
- preserve large sequential write memory bounds by evicting full parts from the in-memory `WriteBuffer` after they are durably written into the local shadow file
- treat these parts as `shadow-backed and evicted`, not as `already uploaded remotely`
- teach the read and flush paths to reload evicted parts from the shadow file when needed

This keeps correctness for unknown-size writes, avoids the invalid `v2_upload_initiate_too_large` behavior, and restores the part-by-part memory release that large sequential writers depend on.

## Context

### Current State

The FUSE write path currently has three relevant pieces:

1. `WriteBuffer.OnPartFull` is wired on create and `O_TRUNC` handles so that a sequential write can react as soon as one full part is produced.
2. `StreamUploader.SubmitPart()` historically initiated multipart upload lazily and started uploading full parts in the background.
3. `Flush()` or `Release()` eventually commits the file through `flushHandle()`.

Verified current ownership:

- create path wiring: `pkg/fuse/dat9fs.go`
- truncate-open path wiring: `pkg/fuse/dat9fs.go`
- multipart helper: `pkg/fuse/stream_upload.go`
- in-memory write tracking and eviction semantics: `pkg/fuse/write.go`
- local shadow file support: `pkg/fuse/shadow.go`

The current codebase already has local shadow infrastructure that is relevant to this problem:

- create and truncate handles can pre-establish a local shadow file through `ShadowStore.Ensure(...)`
- writes can be mirrored to that shadow file through `ShadowStore.WriteAt(...)`
- the shadow file can later be read back through `ShadowStore.ReadAt(...)`

### Problem Statement

The original streaming implementation for create and truncate handles used `SubmitPart()` to lazily initiate multipart upload before the final file size was known. That path used a bogus total size, which produced `v2_upload_initiate_too_large` on the server and was one of the root causes exposed by issue #249.

A narrow fix for that problem is to stop doing remote multipart initiation from `SubmitPart()` for unknown-size handles. That makes the upload correct again, because `Flush()` can later call `UploadAll()` with the exact final size.

However, turning `SubmitPart()` into a no-op leaves the existing `OnPartFull -> onDone -> EvictPart` contract disconnected:

- full parts are no longer evicted from memory during large sequential writes
- create and truncate workloads fall back to retaining the entire file in memory until `Flush()`
- the large-file write path regresses from bounded per-part memory toward O(file size) memory growth

This is not the same bug as issue #249. It does not directly corrupt file contents on the minimal repro path. It is a follow-up execution-model problem that can surface as high memory pressure or failed large writes.

### Constraints

The design must satisfy the following verified constraints:

1. Unknown-size create and truncate handles cannot rely on remote multipart initiation during `Write()`, because the current multipart APIs require an exact total size at initiate time.
2. The create and truncate paths still need per-part memory release for large sequential writes.
3. The system must not reuse the current `uploadedParts` / `HasStreamedParts()` semantics for locally evicted parts, because those flags currently mean `already uploaded remotely`.
4. Reads before final remote commit must continue to work from local state.
5. `Flush()` must not upload zero-filled placeholders for parts that were evicted from memory but are still only present in local shadow storage.

## Goals

1. Prevent create and truncate FUSE handles from issuing remote multipart initiate requests before the final file size is known.
2. Restore bounded-memory behavior for large sequential create and truncate writes.
3. Preserve correct pre-commit read semantics for locally written data.
4. Keep `Flush()` and `Release()` responsible for the first remote multipart initiate for unknown-size handles.

## Non-Goals

- do not reintroduce the old `math.MaxInt64` initiate behavior
- do not redesign the multipart protocol itself
- do not change the existing write-back / commit-queue fix for issue #249
- do not require create and truncate handles to perform real remote streaming before `Flush()`
- do not solve unrelated FUSE overwrite or revision-conflict issues in this proposal

## Design

### 1) Current architecture snapshot

Current behavior for large sequential create or truncate writes:

```text
Write()
  -> WriteBuffer grows in memory
  -> OnPartFull(part)
     -> StreamUploader.SubmitPart(...)
        -> today either:
           a) invalid early remote initiate, or
           b) no-op with no eviction

Flush()/Release()
  -> flushHandle()
     -> UploadAll(total_size, all parts still resident in memory)
```

The follow-up design introduces a distinct local-eviction path:

```text
Write()
  -> WriteBuffer grows in memory
  -> ShadowStore.WriteAt() mirrors bytes locally
  -> OnPartFull(part)
     -> mark part shadow-backed
     -> evict part from WriteBuffer memory

Read() before remote commit
  -> if part not resident but shadow-backed
     -> reload from ShadowStore

Flush()/Release()
  -> first remote multipart initiate with exact total_size
  -> UploadAll() reads resident parts or reloads shadow-backed parts
```

### 2) Introduce a separate `shadow-backed evicted` state

`WriteBuffer.EvictPart()` and `uploadedParts` currently mean:

- the part was already uploaded through the remote streaming path
- subsequent reads and flush logic may treat that part as remotely handled

That meaning is wrong for create and truncate handles whose parts are only durable in the local shadow file.

The follow-up implementation should add a separate state for parts that are:

- no longer resident in memory
- available in local shadow storage
- not yet uploaded remotely

The exact field names can follow local code style, but the state model must keep these concepts separate:

- `remote-streamed part`
- `shadow-backed evicted part`
- `resident in-memory part`

### 3) Evict full parts only after local shadow durability exists

For create and truncate handles, the design should use the existing shadow file as the eviction backing store:

- the handle already establishes a shadow file early
- write-through already mirrors user bytes into that shadow file

When `OnPartFull` fires for a sequentially completed part:

1. verify that the handle still has a valid shadow backing
2. mark that part as shadow-backed and evicted
3. remove the part's bytes from the in-memory `WriteBuffer`

This restores bounded-memory behavior without claiming that the part is remotely uploaded.

If the handle does not have a valid shadow backing, the code must not evict the part. In that degraded path, correctness wins and the part stays resident in memory until `Flush()`.

### 4) Reload shadow-backed parts on read and flush

The current `WriteBuffer.PartData()` behavior returns zero-filled data for unloaded parts that are in range. That is acceptable for some sparse-buffer cases, but it is not acceptable for shadow-backed evicted parts.

The follow-up design therefore needs explicit reload behavior:

- `Read()` must detect shadow-backed evicted parts and repopulate them from `ShadowStore.ReadAt(...)` before serving data
- `Flush()` / `UploadAll()` must detect shadow-backed evicted parts and load exact bytes from the shadow file before uploading

This is required to preserve correctness. Without it, the system would risk uploading zero-filled data for non-resident parts.

### 5) Keep remote multipart initiation at flush time

The proposal does not change the boundary for remote multipart initiation:

- create and truncate handles still do not initiate multipart during `Write()`
- the first remote multipart initiate remains in `Flush()` / `Release()`
- the initiate call must use the exact final file size

That keeps the original #249 fix intact while separating it from the later memory-management enhancement.

### 6) Preserve failure handling and degraded behavior

The design should keep the degraded path simple:

- if shadow backing is unavailable or becomes invalid, stop evicting parts
- continue buffering remaining parts in memory
- let `Flush()` upload from the resident buffer, as it does today

This avoids mixing correctness recovery with aggressive fallback machinery.

## Compatibility and Invariants

The following invariants must hold across the enhancement:

1. Unknown-size create and truncate writes must not emit remote multipart initiate requests during `Write()`.
2. `HasStreamedParts()` must continue to mean `remotely streamed parts exist`, not `some bytes were evicted locally`.
3. A file that is readable before final remote commit must remain readable after this change.
4. `Flush()` must upload exact file bytes, not zero-filled placeholders for evicted parts.
5. The design must not weaken the existing fix for issue #249's write-back fallback path.

## Rollout Plan

- Phase A: separate state and plumbing
  - introduce the distinct shadow-backed eviction state
  - keep existing remote-streaming state unchanged
- Phase B: eviction and reload behavior
  - evict full parts only when shadow backing is valid
  - teach read and flush paths to reload shadow-backed parts
- Phase C: validation and cleanup
  - add focused tests for memory-release semantics, read-after-evict, and flush correctness
  - remove any temporary assumptions left from the no-op `SubmitPart()` path

## Validation Strategy

- unit tests for `WriteBuffer` part-state transitions
- FUSE package tests covering:
  - create sequential large write evicts full parts without remote initiate
  - truncate sequential large write evicts full parts without remote initiate
  - read-after-evict reloads bytes from shadow correctly
  - flush uploads exact bytes after shadow-backed eviction
- local `drive9-server-local` smoke validation for the original #249 repro shape to ensure this enhancement does not regress the existing bugfix
- optional memory-profile or bounded-residency checks for large sequential writes

## Risks and Mitigations

1. Shadow-backed eviction state may drift from existing streamed-part semantics.
   - Mitigation: keep the state model explicit and avoid reusing `uploadedParts` or `HasStreamedParts()` for local eviction.

2. Flush may silently upload incorrect data if evicted parts are not reloaded.
   - Mitigation: require explicit reload logic in the upload path and add tests that verify uploaded bytes exactly match source bytes.

3. Read path complexity may increase for pre-commit local files.
   - Mitigation: keep the fallback narrow; only shadow-backed evicted parts need the reload branch.

4. Shadow file errors could reintroduce brittle behavior.
   - Mitigation: when shadow backing is unavailable, disable further eviction and fall back to in-memory retention rather than risking data loss.

## Alternatives Considered

### 1. Restore the old `SubmitPart()` remote streaming behavior

Rejected because it reintroduces the invalid early multipart initiate problem for unknown-size handles.

### 2. Keep the no-op `SubmitPart()` implementation permanently

Rejected because it leaves large sequential create and truncate writes with O(file size) memory growth.

### 3. Introduce a brand-new local staging subsystem

Rejected for now because the repository already has `ShadowStore`, and reusing that existing durability boundary is the smaller production-safe design.
