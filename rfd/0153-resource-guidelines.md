---
authors: Tim Ross (tim.ross@goteleport.com)
state: draft
---

# RFD 153 - Resource Guidelines

## Required approvers

- Engineering: `@zmb3`

## What

Guidelines for creating backend resources and APIs to interact with them.

## Why

To date resources follow slightly different patterns and have inconsistent APIs for creating, updating and deleting
them, see [#29234](https://github.com/gravitational/teleport/issues/29234). In addition to reducing confusion and
fostering a better developer experience, this will also remove complexity from the terraform provider and teleport
operator that have to accommodate for every subtle API and resource difference that exist today.

## Details

### Defining a resource

A resource at minimum MUST include a kind, version, `teleport.header.v1.Metadata` and a specification message with any
additional parameters needed to represent the resource. The kind, version and metadata helps to identify the type of
resource, when it should expire, the version of the resource, etc. This is particularly important for ensuring that all
resources contain the same shape and that tools like `tctl` are able to leverage `services.UnknownResource` to identify
the resource and act appropriately.

```protobuf
import "teleport/header/v1/resourceheader.proto";

// Foo is a resource that does foo.
message Foo {
  // The kind of resource represented.
  string kind = 1;
  // An optional subkind to differentiate variations of the same kind.
  string sub_kind = 2;
  // The version of the resource being represented.
  string version = 3;
  // Common metadata that all resources shared.
  teleport.header.v1.Metadata metadata = 4;
  // The resource specific specification.
  FooSpec spec = 5;
}

// FooSpec contains resource specific properties.
message FooSpec {
  string bar = 1;
  int32 baz = 2;
  bool qux = 3;
}
```

This differs from existing resources because legacy resources make heavy use of features provided
by [gogoprotobuf](https://github.com/gogo/protobuf). Since that project has long been abandoned, we're striving to
migrate away from it as described in [RFD-0139](https://github.com/gravitational/teleport/pull/28386).
The `teleport.header.v1.Metadata` is a clone of `types.Metadata` which doesn't use any of the gogoproto features.
Legacy resources also had a `types.ResourceHeader` that used gogo magic to embed the type in the resource message. To
get around this, the required fields from the header MUST be included in the message itself. A non-gogo clone does exist
`teleport.header.v1.ResourceHeader`, however, to get the fields, embeded custom marshalling must be manually written.

If a resource has associated secrets (password, private key, jwt, mfa device, etc.) they should be defined in a separate
resource and stored in a separate key range in the backend. The traditional pattern of defining secrets inline and only
returning them if a `with_sercrets` flag is provided cause a variety of problems and introduce opportunity for human
error to accidentally include secrets when they should not have been. It would then be the responsibility of the caller
to get both the base resource and the corresponding secret resource if required.

### API

All APIs should follow the following conventions that are largely based on
the [Google API style guide](https://cloud.google.com/apis/design/standard_methods).

#### Create

The `Create` RPC takes a resource to be created and must also return the newly created resource so that any fields that
are populated server side are provided to clients without requiring an additional call to `Get`.

The request MUST fail and return a `trace.AlreadyExists` error if a matching resource is already present in the backend.

```protobuf
// Creates a new Foo resource in the backend.
    rpc CreateFoo(CreateFooRequest) returns (CreateFooResponse);

message CreateFooRequest {
  // The desired Foo to be created.
  Foo foo = 1;
}

message CreateFooResponse {
  // The created Foo as it exists in the backend. The only differences between the returned
  // resource and the requested resource are due to properties that are updated by the server.
  // In most cases this will be limited to the Revision field of the Metadata in the ResourceHeader.
  Foo foo = 1;
}
```

#### Update

The `Update` RPC takes a resource to be updated and must also return the updated resource so that any fields that are
populated server side are provided to clients without requiring an additional call to `Get`. If partial updates of a
resource are desired, the request may contain
a [FieldMask](https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#fieldmask).

The request MUST fail and return a `trace.NotFound` error if there is no matching resource in the backend.

```protobuf
// Updates an existing Foo in the backend.
    rpc UpdateFoo(UpdateFooRequest) returns (UpdateFooResponse);

message UpdateFooRequest {
  // The full Foo resource to update in the backend.
  Foo foo = 1;
  // A partial update for an existing Foo resource.
  FieldMask update_mask = 2;
}

message UpdateFooResponse {
  // The updated Foo as it exists in the backend. The only differences between the returned
  // resource and the requested resource are due to properties that are updated by the server.
  // In most cases this will be limited to the Revision field of the Metadata in the ResourceHeader.
  Foo foo = 1;
}
```

#### Upsert

> The `Create` and `Update` RPCs should be preferred over `Upsert` for normal operations,
> see [#1326](https://github.com/gravitational/teleport/issues/1326) for more details.

The `Upsert` RPC takes a resource that will overwrite a matching existing resource or create a new resource if one does
not exist. The upserted resource is returned so that any fields that are populated server side are provided to clients
without requiring a call to `Get`. If `Upsert` is not consumed it may be omitted from the API in favor of `Create` and
`Update`.

```protobuf
// Creates a new Foo or updates an existing Foo in the backend.
    rpc UpsertFoo(UpsertFooRequest) returns (UpsertFooResponse);

message UpsertFooRequest {
  // The full Foo resource to persist in the backend.
  Foo foo = 2;
}

message UpsertFooResponse {
  // The upserted Foo as it exists in the backend. The only differences between the returned
  // resource and the requested resource are due to properties that are updated by the server.
  // In most cases this will be limited to the Revision field of the Metadata in the ResourceHeader.
  Foo foo = 1;
}
```

#### Get

The `Get` RPC takes the parameters required to match a resource (usually the resource name should suffice), and returns
the matched resource.

The request MUST fail and return a `trace.NotFound` error if there is no matching resource in the backend.

```protobuf
// Returns a single Foo matching the request
    rpc GetFoo(GetFooRequest) returns (GetFooResponse);

message GetFooRequest {
  // A filter to match the Foo by. Some resource may require more parameters to match and
  // may not use the name at all.
  string name = 1;
}

message GetFooResponse {
  // The matched Foo resource.
  Foo foo = 1;
}
```

#### List

The `List` RPC takes the requested page size and starting point and returns a list of resources that match. If there are
additional resources, the response MUST also include a token that indicates where the next page of results begins.

Most legacy APIs do not provide a paginated way to retrieve resources and instead offer some kind of `GetAllFoos` RPC
which either returns all `Foo` in a single message or leverages a server side stream to send each `Foo` one at a time.
Returning all items in a single message causes problems when the number of resources scales beyond gRPC message size
limits. Due to caches making use of the `GetAll` RPC variants to populate themselves during initialization this can lead
to permanently unhealthy caches constantly retrying to initialize which can lead to backend throttling and enough
increased load on Auth to render the cluster unusable.

```protobuf
// Returns a page of Foo and the token to find the next page of items.
    rpc ListFoo(ListFooRequest) returns (ListFooResponse);

message ListFoosRequest {
  // The maximum number of items to return.
  // The server may impose a different page size at its discretion.
  int32 page_size = 1;
  // The next_page_token value returned from a previous List request, if any.
  string page_token = 2;
}

message ListFoosResponse {
  // The page of Foo that matched the request.
  repeated Foo foos = 1;
  // Token to retrieve the next page of results, or empty if there are no
  // more results in the list.
  string next_page_token = 2;
}
```

A listing operation should not abort entirely if a single item cannot be (un)marshalled, it should instead be logged,
and the rest of the page should be processed. Aborting an entire page when a single entry is invalid causes the cache
to be permanently unhealthy since it is never able to initialize loading the affected resource.

#### Delete

The `Delete` RPC takes the parameters required to match a resource (usually the resource name should suffice), and
removes the specified resource from the backend and returns a `google.protobuf.Empty`.

The request MUST fail and return a `trace.NotFound` error if there is no matching resource in the backend.

```protobuf
// Remove a matching Foo resource
    rpc DeleteFoo(DeleteFooRequest) returns (google.protobuf.Empty);

message DeleteFooRequest {
  // Name of the foo to remove. Some resource may require more parameters to match and
  // may not use the name at all.
  string name = 1;
}
```

### Backend Storage

A backend service to handle persisting and retrieving a resource from the backend is typically defined in
`lib/services/local/foo.go`. An accompanying interface which mirrors the service is defined in `lib/services/foo.go`.
Continuing on with the example above, the backend service for the `Foo` resource might look like the following.

#### Create

When creating a new resource, the `backend.Backend.Create` method should be used to persist the resource. It is also
imperative that the revision generated by the backend is set on the returned resource.

```go
func (s *FooService) CreateFoo(ctx context.Context, foo *foov1.Foo) (*foov1.Foo, error) {
	value, err := convertFooToValue(foo)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	item := backend.Item{
		Key:     backend.Key("foo", foo.GetName()),
		Value:   value,
		Expires: foo.Expiry(),
	}

	lease, err := s.backend.Create(ctx, item)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Update the foo with the revision generated by the backend during the write operation.
	foo.SetRevision(lease.Revision)
	return foo, nil
}
```

#### Update

All update operations should prefer `backend.Backend.ConditionalUpdate` over the `backend.Backend.Update` method to
prevent blindly overwriting an existing item. When using conditional update, the backend write will only succeed if the
revision of the resource in the update request matches the revision of the item in the backend. Conditional updates
should also be preferred over traditional `CompareAndSwap` operations.

```go
func (s *FooService) UpdateFoo(ctx context.Context, foo *foov1.Foo) (*foov1.Foo, error) {
	// The revision is cached prior to converting to the value because 
	// conversion functions may set the revision to "" if MarshalConfig.PreserveResourceID
	// is not set.
	rev := foo.GetRevision()
	value, err := convertFooToValue(foo)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	item := backend.Item{
		Key:     backend.Key("foo", foo.GetName()),
		Value:   value,
		Expires: foo.Expiry(),
		Revision: rev,
	}

	lease, err := s.backend.ConditionalUpdate(ctx, item)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Update the foo with the revision generated by the backend during the write operation.
	foo.SetRevision(lease.Revision)
	return foo, nil
}
```

#### Upsert

When upserting a resource, the `backend.Backend.Put` method should be used to persist the resource. It is also
imperative that the revision generated by the backend is set on the returned resource.

A resource may expose an upsert method from the backend layer even if the gRPC API does not expose an `Upsert` RPC. This
may occur if a resource is cached, since the
[cache collections](https://github.com/gravitational/teleport/blob/004d0db0c1f6e9b312d0b0e1330b6e5bf1ffef6e/lib/cache/collections.go#L60)
require an upsert mechanism,
see [`services.DynamicAccessExt`](https://github.com/gravitational/teleport/blob/004d0db0c1f6e9b312d0b0e1330b6e5bf1ffef6e/lib/services/access_request.go#L260-L278)
for an example.

```go
func (s *FooService) UpsertFoo(ctx context.Context, foo *foov1.Foo) (*foov1.Foo, error) {
	value, err := convertFooToValue(foo)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	item := backend.Item{
		Key:     backend.Key("foo", foo.GetName()),
		Value:   value,
		Expires: foo.Expiry(),
	}

	lease, err := s.backend.Put(ctx, item)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Update the foo with the revision generated by the backend during the write operation.
	foo.SetRevision(lease.Revision)
	return foo, nil
}
```

#### Get

To retrieve a resource the `backend.Backend.Get` method should be provided a key built from the match parameters of the
request.

```go
func (s *AccessService) GetFoo(ctx context.Context, name string) (*Foo, error) {
	if name == "" {
		return nil, trace.BadParameter("missing foo name")
	}
	
	item, err := s.backend.Get(ctx, backend.Key("foo", name))
	if err != nil {
		if trace.IsNotFound(err) {
			return nil, trace.NotFound("foo %v is not found", name)
		}
		return nil, trace.Wrap(err)
	}
	foo, err := convertItemToFoo(item)
	return foo, trace.Wrap(err)
}
```

#### List

Listing can either be done via calling `backend.Backend.GetRange` manually in a loop or by making use of the functional
helpers in the
[stream](https://github.com/gravitational/teleport/blob/004d0db0c1f6e9b312d0b0e1330b6e5bf1ffef6e/api/internalutils/stream/stream.go)
package to do the heavy lifting.

A listing operation should not abort entirely if a single item cannot be converted from a `backend.Item`, it should
instead be logged, and the rest of the page should be processed. Aborting an entire page when a single entry is invalid,
causes Teleport to be permanently unhealthy since it is never able to load or cache the affected resource(s).

```go
func (s *FooService) ListFoos(ctx context.Context, pageSize int, pageToken string) ([]*foov1.Foo, string, error) {
	rangeStart := backend.Key("foo", pageToken)
	rangeEnd := backend.RangeEnd(backend.ExactKey("foo"))

	// Adjust page size, so it can't be too large.
	if pageSize <= 0 || pageSize > apidefaults.DefaultChunkSize {
		pageSize = apidefaults.DefaultChunkSize
	}

	// Increase the page size by one to detect if another page is available if
	// a full page match is retrieved without having to fetch the next page.
	pagSize++

	fooStream := stream.MapWhile(
		backend.StreamRange(ctx, s.backend, rangeStart, rangeEnd, limit),
		func (item backend.Item) (types.User, bool) {
			foo, err := convertItemToFoo(item)

			// Warn if an item cannot be converted but don't prevent the entire page from being processed.
			if err != nil {
				s.log.Warnf("Skipping foo at %s because conversion from backend item failed: %v", item.Key, err)
				return nil, true
			}
			return foo, true
		}
	)

	foos, more := stream.Take(userStream, pageSize)
	var nextToken string
	if more && fooStream.Next() {
		nextToken = backend.NextPaginationKey(foos[len(foos)-1])
	}

	return foos, nextToken, trace.NewAggregate(err, fooStream.Done())
}
```

### Cache

One thing to consider when creating a resource is whether it will need to be cached. As mentioned above, any resource
that is cached must have its backend layer implement the specific set of operations required by the cache collections
[executor](https://github.com/gravitational/teleport/blob/004d0db0c1f6e9b312d0b0e1330b6e5bf1ffef6e/lib/cache/collections.go#L54-L76).
To avoid including `Upsert` or `DeleteAll` in the gRPC API if they are only consumed by the cache is to define a local
variant of the backend service similar to
[`services.DynamicAccessExt`](https://github.com/gravitational/teleport/blob/004d0db0c1f6e9b312d0b0e1330b6e5bf1ffef6e/lib/services/access_request.go#L260-L278).

Not all resources need to be cached. If a resource is infrequently accessed outside the hot path, then adding it to the
cache is probably not necessary. It is also discouraged to add a resource to the cache if it scales linearly with the
size of the cluster. If a resource is to be cached, it must be added to the
[Auth cache](https://github.com/gravitational/teleport/blob/004d0db0c1f6e9b312d0b0e1330b6e5bf1ffef6e/lib/cache/cache.go#L95-L154)
and the cache of any service that requires it.

If the `Foo` resource was to be cached its executor would look similar to the following:

```go
type fooExecutor struct{}

func (fooExecutor) getAll(ctx context.Context, cache *Cache, loadSecrets bool) ([]*foov1.Foo, error) {
	var (
		startKey string
		allFoos []*foov1.Foo
	)
	for {
		foos, nextKey, err := cache.Foo.ListFoos(ctx, 0, startKey, "")
		if err != nil {
			return nil, trace.Wrap(err)
		}

		allFoos = append(allFoos, foos...)

		if nextKey == "" {
			break
		}
		startKey = nextKey
	}
	return allFoos, nil
}

func (fooExecutor) upsert(ctx context.Context, cache *Cache, resource foov1.Foo) error {
	return cache.Foo.UpsertFoo(ctx, resource)
}

func (fooExecutor) deleteAll(ctx context.Context, cache *Cache) error {
	return cache.FooLocal.DeleteAllFoos(ctx)
}

func (fooExecutor) delete(ctx context.Context, cache *Cache, resource types.Resource) error {
	return cache.Foo.DeleteFoo(ctx, &foov1.DeleteFoo{Name: resource.GetName()})
}
```

### Proto Specification

Below is the entire specification for the examples above.

<details open><summary>Foo Proto</summary>

```protobuf
syntax = "proto3";

package teleport.foo.v1;

import "teleport/header/v1/resourceheader.proto";

option go_package = "github.com/gravitational/teleport/api/gen/proto/go/teleport/foo/v1;foov1";

// Foo is a resource that does foo.
message Foo {
  // Header contains the common metadata that all resources must have.
  teleport.header.v1.ResourceHeader header = 1;

  // Define the resource specific properties below. The ones
  // defined here are just examples.
  string bar = 2;
  int32 baz = 3;
  bool qux = 4;
}

// FooService provides an API to manage Foos.
service FooService {
  // GetFoo returns the specified Foo resource.
  rpc GetFoo(GetFooRequest) returns (GetFooResponse);

  // ListFoo returns a page of Foo resources.
  rpc ListFoo(ListFooRequest) returns (ListFooResponse);

  // CreateFoo creates a new Foo resource.
  rpc CreateFoo(CreateFooRequest) returns (CreateFooResponse);

  // UpdateFoo updates an existing Foo resource.
  rpc UpdateFoo(UpdateFooRequest) returns (UpdateFooResponse);

  // UpsertFoo creates or updates a Foo resource.
  rpc UpsertFoo(UpsertFooRequest) returns (UpsertFooResponse);

  // DeleteFoo removes the specified Foo resource.
  rpc DeleteFoo(DeleteFooRequest) returns (google.protobuf.Empty);
}

// Request for GetFoo.
message GetFooRequest {
  // The name of the Foo resource to retrieve.
  string name = 1;
}

// Response for GetFoo.
message GetFooResponse {
  // The foo matching the request filters.
  Foo foo = 1;
}

// Request for ListFoos.
//
// Follows the pagination semantics of
// https://cloud.google.com/apis/design/standard_methods#list.
message ListFoosRequest {
  // The maximum number of items to return.
  // The server may impose a different page size at its discretion.
  int32 page_size = 1;

  // The page_token value returned from a previous ListFoo request, if any.
  string page_token = 2;
}

// Response for ListFoos.
message ListFoosResponse {
  // Foo that matched the search.
  repeated Foo foos = 1;

  // Token to retrieve the next page of results, or empty if there are no
  // more results exist.
  string next_page_token = 2;
}

// Request for CreateFoo.
message CreateFooRequest {
  // The foo resource to create.
  Foo foo = 1;
}

// Response for CreateFoo.
message CreateFooResponse {
  // The created foo with any server side generated fields populated.
  Foo foo = 1;
}

// Request for UpdateFoo.
message UpdateFooRequest {
  // The foo resource to update.
  Foo foo = 2;

  // The update mask applied to a Foo.
  // Fields are masked according to their proto name.
  FieldMask update_mask = 2;
}

// Response for UpdateFoo.
message UpdateFooResponse {
  // The updated foo with any server side generated fields populated.
  Foo foo = 1;
}

// Request for UpsertFoo.
message UpsertFooRequest {
  // The foo resource to upsert.
  Foo foo = 2;
}

// Response for UpsertFoo.
message UpsertFooResponse {
  // The upserted foo with any server side generated fields populated.
  Foo foo = 1;
}

// Request for DeleteFoo.
message DeleteFooRequest {
  // Name of the foo to remove.
  string name = 1;
}
```

</details>

### Backward Compatibility

Changing existing resources which do not follow the guidelines laid out in this RFD may lead to breaking changes. It is
not recommended to change existing resources for change’s sake. Migrating APIs which do not conform to the
recommendations in this RFD can be made in a backward compatible manner. This can be achieved by adding new APIs that
conform with the advice above and falling back to the existing APIs if a `trace.NotImplemented` error is received. Once
all compatible versions of Teleport are using the new version of the API, the old API may be cleaned up.
