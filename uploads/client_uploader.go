package uploads

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"www.velocidex.com/golang/velociraptor/accessors"
	actions_proto "www.velocidex.com/golang/velociraptor/actions/proto"
	"www.velocidex.com/golang/velociraptor/constants"
	crypto_proto "www.velocidex.com/golang/velociraptor/crypto/proto"
	"www.velocidex.com/golang/velociraptor/responder"
	"www.velocidex.com/golang/vfilter"
)

var (
	BUFF_SIZE  = int64(1024 * 1024)
	UPLOAD_CTX = "__uploads"
)

// An uploader delivering files from client to server.
type VelociraptorUploader struct {
	Responder responder.Responder
	Count     int
}

func (self *VelociraptorUploader) Upload(
	ctx context.Context,
	scope vfilter.Scope,
	filename *accessors.OSPath,
	accessor string,
	store_as_name *accessors.OSPath,
	expected_size int64,
	mtime time.Time,
	atime time.Time,
	ctime time.Time,
	btime time.Time,
	reader io.Reader) (
	*UploadResponse, error) {

	if store_as_name == nil {
		store_as_name = filename
	}

	cached, pres := DeduplicateUploads(scope, store_as_name)
	if pres {
		return cached, nil
	}

	upload_id := self.Responder.NextUploadId()

	// Try to collect sparse files if possible
	result, err := self.maybeUploadSparse(
		ctx, scope, filename, accessor, store_as_name,
		expected_size, mtime, upload_id, reader)
	if err == nil {
		CacheUploadResult(scope, store_as_name, result)
		return result, nil
	}

	result = &UploadResponse{
		Path:       filename.String(),
		StoredName: store_as_name.String(),
		Accessor:   accessor,
		Components: store_as_name.Components[:],
	}

	offset := uint64(0)
	self.Count += 1

	md5_sum := md5.New()
	sha_sum := sha256.New()

	BUFF_SIZE = int64(1024 * 1024)

	for {
		// Ensure there is a fresh allocation for every
		// iteration to prevent overwriting in flight buffers.
		buffer := make([]byte, BUFF_SIZE)
		read_bytes, err := reader.Read(buffer)
		if err != nil && err != io.EOF {
			return nil, err
		}

		data := buffer[:read_bytes]
		_, err = sha_sum.Write(data)
		if err != nil {
			return nil, err
		}

		_, err = md5_sum.Write(data)
		if err != nil {
			return nil, err
		}

		packet := &actions_proto.FileBuffer{
			Pathspec: &actions_proto.PathSpec{
				Path:       store_as_name.String(),
				Components: store_as_name.Components,
				Accessor:   accessor,
			},
			Offset:     offset,
			Size:       uint64(expected_size),
			StoredSize: uint64(expected_size),
			Mtime:      mtime.UnixNano(),
			Atime:      atime.UnixNano(),
			Ctime:      ctime.UnixNano(),
			Btime:      btime.UnixNano(),
			Data:       data,

			// The number of the upload within the flow.
			UploadNumber: upload_id,
			Eof:          read_bytes == 0,
		}

		select {
		case <-ctx.Done():
			return nil, errors.New("Cancelled!")

		default:
			// Send the packet to the server.
			self.Responder.AddResponse(&crypto_proto.VeloMessage{
				RequestId:  constants.TransferWellKnownFlowId,
				FileBuffer: packet})
		}

		offset += uint64(read_bytes)
		if err != nil && err != io.EOF {
			return nil, err
		}

		// On the last packet send back the hashes into the query.
		if read_bytes == 0 {
			result.Size = offset
			result.StoredSize = offset
			result.Sha256 = hex.EncodeToString(sha_sum.Sum(nil))
			result.Md5 = hex.EncodeToString(md5_sum.Sum(nil))

			CacheUploadResult(scope, store_as_name, result)
			return result, nil
		}
	}
}

func (self *VelociraptorUploader) maybeUploadSparse(
	ctx context.Context,
	scope vfilter.Scope,
	filename *accessors.OSPath,
	accessor string,
	store_as_name *accessors.OSPath,
	ignored_expected_size int64,
	mtime time.Time,
	upload_id int64,
	reader io.Reader) (
	*UploadResponse, error) {

	// Can the reader produce ranges?
	range_reader, ok := reader.(RangeReader)
	if !ok {
		return nil, errors.New("Not supported")
	}

	index := &actions_proto.Index{}

	if store_as_name == nil {
		store_as_name = filename
	}

	// This is the response that will be passed into the VQL
	// engine.
	result := &UploadResponse{
		Path:       filename.String(),
		StoredName: store_as_name.String(),
		Components: store_as_name.Components,
		Accessor:   accessor,
	}

	self.Count += 1

	md5_sum := md5.New()
	sha_sum := sha256.New()

	// Does the index contain any sparse runs?
	is_sparse := false

	// Read from the sparse file with read_offset and write to the
	// output file at write_offset. All ranges are written back to
	// back skipping sparse ranges. The index file will allow
	// users to reconstruct the sparse file if needed.
	read_offset := int64(0)
	write_offset := int64(0)

	// Adjust the expected size properly to the sum of all
	// non-sparse ranges and build the index file.
	ranges := range_reader.Ranges()

	// Inspect the ranges and prepare an index.
	expected_size := int64(0)
	real_size := int64(0)
	for _, rng := range ranges {
		file_length := rng.Length
		if rng.IsSparse {
			file_length = 0
		}

		index.Ranges = append(index.Ranges,
			&actions_proto.Range{
				FileOffset:     expected_size,
				OriginalOffset: rng.Offset,
				FileLength:     file_length,
				Length:         rng.Length,
			})

		if !rng.IsSparse {
			expected_size += rng.Length
		} else {
			is_sparse = true
		}

		if real_size < rng.Offset+rng.Length {
			real_size = rng.Offset + rng.Length
		}
	}

	// No ranges - just send a placeholder.
	if expected_size == 0 {
		if !is_sparse {
			index = nil
		}

		self.Responder.AddResponse(&crypto_proto.VeloMessage{
			RequestId: constants.TransferWellKnownFlowId,
			FileBuffer: &actions_proto.FileBuffer{
				Pathspec: &actions_proto.PathSpec{
					Path:       store_as_name.String(),
					Components: store_as_name.Components,
					Accessor:   accessor,
				},
				Size:         uint64(real_size),
				StoredSize:   0,
				IsSparse:     is_sparse,
				Index:        index,
				Mtime:        mtime.UnixNano(),
				Eof:          true,
				UploadNumber: upload_id,
			},
		})

		result.Size = uint64(real_size)
		result.Sha256 = hex.EncodeToString(sha_sum.Sum(nil))
		result.Md5 = hex.EncodeToString(md5_sum.Sum(nil))
		return result, nil
	}

	// Send each range separately
	for _, rng := range ranges {
		// Ignore sparse ranges
		if rng.IsSparse {
			continue
		}

		// Range is not sparse - send it one buffer at the time.
		to_read := rng.Length
		read_offset = rng.Offset
		_, err := range_reader.Seek(read_offset, io.SeekStart)
		if err != nil {
			return nil, err
		}

		for to_read > 0 {
			to_read_buf := to_read

			// Ensure there is a fresh allocation for every
			// iteration to prevent overwriting in-flight buffers.
			if to_read_buf > BUFF_SIZE {
				to_read_buf = BUFF_SIZE
			}

			buffer := make([]byte, to_read_buf)
			read_bytes, err := range_reader.Read(buffer)
			// Hard read error - give up.
			if err != nil && err != io.EOF {
				return nil, err
			}

			// End of range - go to the next range
			if read_bytes == 0 || err == io.EOF {
				to_read = 0
				continue
			}

			data := buffer[:read_bytes]
			_, err = sha_sum.Write(data)
			if err != nil {
				return nil, err
			}

			_, err = md5_sum.Write(data)
			if err != nil {
				return nil, err
			}

			packet := &actions_proto.FileBuffer{
				Pathspec: &actions_proto.PathSpec{
					Path:       store_as_name.String(),
					Components: store_as_name.Components,
					Accessor:   accessor,
				},
				Offset:       uint64(write_offset),
				Size:         uint64(real_size),
				StoredSize:   uint64(expected_size),
				IsSparse:     is_sparse,
				Mtime:        mtime.UnixNano(),
				Data:         data,
				UploadNumber: upload_id,
			}

			select {
			case <-ctx.Done():
				return nil, errors.New("Cancelled!")

			default:
				// Send the packet to the server.
				self.Responder.AddResponse(&crypto_proto.VeloMessage{
					RequestId:  constants.TransferWellKnownFlowId,
					FileBuffer: packet})
			}

			to_read -= int64(read_bytes)
			write_offset += int64(read_bytes)
			read_offset += int64(read_bytes)
		}
	}

	// We did a sparse file, upload the index as well.
	if !is_sparse {
		index = nil
	}

	// Send an EOF as the last packet with no data. If the file
	// was sparse, also include the index in this packet. NOTE:
	// There should be only one EOF packet.
	self.Responder.AddResponse(&crypto_proto.VeloMessage{
		RequestId: constants.TransferWellKnownFlowId,
		FileBuffer: &actions_proto.FileBuffer{
			Pathspec: &actions_proto.PathSpec{
				Path:       store_as_name.String(),
				Components: store_as_name.Components,
				Accessor:   accessor,
			},
			Size:         uint64(real_size),
			StoredSize:   uint64(expected_size),
			IsSparse:     is_sparse,
			Offset:       uint64(write_offset),
			Index:        index,
			Eof:          true,
			UploadNumber: upload_id,
		},
	})

	result.Size = uint64(real_size)
	result.StoredSize = uint64(write_offset)
	result.Sha256 = hex.EncodeToString(sha_sum.Sum(nil))
	result.Md5 = hex.EncodeToString(md5_sum.Sum(nil))

	return result, nil
}
