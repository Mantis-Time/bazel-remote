package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	grpc_status "google.golang.org/grpc/status"

	asset "github.com/buchgr/bazel-remote/v2/genproto/build/bazel/remote/asset/v1"
	pb "github.com/buchgr/bazel-remote/v2/genproto/build/bazel/remote/execution/v2"

	"github.com/buchgr/bazel-remote/v2/cache"
)

// FetchServer implementation

var errNilFetchBlobRequest = grpc_status.Error(codes.InvalidArgument,
	"expected a non-nil *FetchBlobRequest")

func (s *grpcServer) FetchBlob(ctx context.Context, req *asset.FetchBlobRequest) (*asset.FetchBlobResponse, error) {

	var sha256Str string

	// Q: which combinations of qualifiers to support?
	// * simple file, identified by sha256 SRI AND/OR recognisable URL
	// * git repository, identified by ???
	// * go repository, identified by tag/branch/???

	// "strong" identifiers:
	// checksum.sri -> direct lookup for sha256 (easy), indirect lookup for
	//     others (eg sha256 of the SRI hash).
	// vcs.commit + .git extension -> indirect lookup? or sha1 lookup?
	//     But this could waste a lot of space.
	//
	// "weak" identifiers:
	// vcs.branch + .git extension -> indirect lookup, with timeout check
	//    directory: limit one of the vcs.* returns
	//               insert to tree into the CAS?
	//
	//    git archive --format=tar --remote=http://foo/bar.git ref dir...

	// For TTL items, we need another (persistent) index, eg BadgerDB?
	// key -> CAS sha256 + timestamp
	// Should we place a limit on the size of the index?

	if req == nil {
		return nil, errNilFetchBlobRequest
	}

	// log the request
	s.accessLogger.Printf("GRPC ASSET FETCH %v", req)

	// log the request qualifiers
	s.accessLogger.Printf("GRPC ASSET FETCH %v", req.GetQualifiers())
	for _, q := range req.GetQualifiers() {
		if q == nil {
			return &asset.FetchBlobResponse{
				Status: &status.Status{
					Code:    int32(codes.InvalidArgument),
					Message: "unexpected nil qualifier in FetchBlobRequest",
				},
			}, nil
		}

		if q.Name == "checksum.sri" && strings.HasPrefix(q.Value, "sha256-") {
			// Ref: https://developer.mozilla.org/en-US/docs/Web/Security/Subresource_Integrity

			b64hash := strings.TrimPrefix(q.Value, "sha256-")

			decoded, err := base64.StdEncoding.DecodeString(b64hash)
			if err != nil {
				s.errorLogger.Printf("failed to base64 decode \"%s\": %v",
					b64hash, err)
				continue
			}

			sha256Str = hex.EncodeToString(decoded)

			found, size := s.cache.Contains(ctx, cache.CAS, sha256Str, -1)
			if !found {
				s.errorLogger.Printf("CAS for uri %s sha %s not found in cache", req.GetUris(), sha256Str)
				continue
			}

			if size < 0 {
				// We don't know the size yet (bad http backend?).
				r, actualSize, err := s.cache.Get(ctx, cache.CAS, sha256Str, -1, 0)
				if r != nil {
					defer r.Close()
				}
				if err != nil || actualSize < 0 {
					s.errorLogger.Printf("failed to get CAS %s from proxy backend size: %d err: %v",
						sha256Str, actualSize, err)
					continue
				}
				size = actualSize
			}

			return &asset.FetchBlobResponse{
				Status: &status.Status{Code: int32(codes.OK)},
				BlobDigest: &pb.Digest{
					Hash:      sha256Str,
					SizeBytes: size,
				},
			}, nil
		}
	}

	if sha256Str == "" {
		// log the condition
		s.errorLogger.Printf("SHA for uri %s not set; Hence defaulting to the URIs", req.GetUris())
		for _, uri := range req.GetUris() {
			hashBytes := sha256.Sum256([]byte(uri))
			hashStr := hex.EncodeToString(hashBytes[:])
			found, size := s.cache.Contains(ctx, cache.CAS, hashStr, -1)
			if !found {
				s.errorLogger.Printf("CAS for uri %s sha %s not found in cache", req.GetUris(), sha256Str)
				continue
			}

			if size < 0 {
				// We don't know the size yet (bad http backend?).
				r, actualSize, err := s.cache.Get(ctx, cache.CAS, hashStr, -1, 0)
				if r != nil {
					defer r.Close()
				}
				if err != nil || actualSize < 0 {
					s.errorLogger.Printf("failed to get CAS %s from proxy backend size: %d err: %v",
						uri, actualSize, err)
					continue
				}
				size = actualSize
			}

			return &asset.FetchBlobResponse{
				Status: &status.Status{Code: int32(codes.OK)},
				BlobDigest: &pb.Digest{
					Hash:      sha256Str,
					SizeBytes: size,
				},
			}, nil
		}
	}

	// Cache miss.

	// See if we can download one of the URIs.

	for _, uri := range req.GetUris() {
		// sha256 := sha256Str
		// if sha256 == "" {
		// 	s.errorLogger.Printf("SHA for uri %s not set; Hence defaulting to the URI", uri)
		// 	sha256 = uri
		// }
		ok, actualHash, size := s.fetchItem(ctx, uri, sha256Str)
		if ok {
			return &asset.FetchBlobResponse{
				Status: &status.Status{Code: int32(codes.OK)},
				BlobDigest: &pb.Digest{
					Hash:      actualHash,
					SizeBytes: size,
				},
				Uri: uri,
			}, nil
		}

		// Not a simple file. Not yet handled...
	}

	return &asset.FetchBlobResponse{
		Status: &status.Status{Code: int32(codes.NotFound)},
	}, nil
}

func (s *grpcServer) fetchItem(ctx context.Context, uri string, expectedHash string) (bool, string, int64) {
	u, err := url.Parse(uri)
	if err != nil {
		s.errorLogger.Printf("unable to parse URI: %s err: %v", uri, err)
		return false, "", int64(-1)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		s.errorLogger.Printf("unsupported URI: %s", uri)
		return false, "", int64(-1)
	}

	resp, err := http.Get(uri)
	if err != nil {
		s.errorLogger.Printf("failed to get URI: %s err: %v", uri, err)
		return false, "", int64(-1)
	}
	defer resp.Body.Close()
	rc := resp.Body

	s.accessLogger.Printf("GRPC ASSET FETCH %s %s", uri, resp.Status)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", int64(-1)
	}

	expectedSize := resp.ContentLength
	if expectedHash == "" || expectedSize < 0 {
		// We can't call Put until we know the hash and size.

		s.accessLogger.Printf("expectedSize: %d", expectedSize)
		s.accessLogger.Printf("expectedHash: %s", expectedHash)
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			s.errorLogger.Printf("failed to read data: %v", uri)
			return false, "", int64(-1)
		}

		expectedSize = int64(len(data))
		hashBytes := sha256.Sum256(data)
		hashStr := hex.EncodeToString(hashBytes[:])

		if expectedHash != "" && hashStr != expectedHash {
			s.errorLogger.Printf("URI data has hash %s, expected %s",
				hashStr, expectedHash)
			return false, "", int64(-1)
		}

		if expectedHash == "" {
			hashBytes := sha256.Sum256([]byte(uri))
			expectedHash = hex.EncodeToString(hashBytes[:])
		}

		// 		expectedHash = hashStr
		rc = io.NopCloser(bytes.NewReader(data))
	}

	err = s.cache.Put(ctx, cache.CAS, expectedHash, expectedSize, rc)
	if err != nil && err != io.EOF {
		s.errorLogger.Printf("failed to Put %s: %v", expectedHash, err)
		return false, "", int64(-1)
	}

	return true, expectedHash, expectedSize
}

func (s *grpcServer) FetchDirectory(context.Context, *asset.FetchDirectoryRequest) (*asset.FetchDirectoryResponse, error) {
	return nil, nil
}

/* PushServer implementation
func (s *grpcServer) PushBlob(context.Context, *asset.PushBlobRequest) (*asset.PushBlobResponse, error) {
	return nil, nil
}

func (s *grpcServer) PushDirectory(context.Context, *asset.PushDirectoryRequest) (*asset.PushDirectoryResponse, error) {
	return nil, nil
}
*/
