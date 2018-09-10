package s3api

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/chrislusf/seaweedfs/weed/filer2"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/satori/go.uuid"
)

func (s3a *S3ApiServer) createMultipartUpload(input *s3.CreateMultipartUploadInput) (output *s3.CreateMultipartUploadOutput, code ErrorCode) {
	uploadId, _ := uuid.NewV4()
	uploadIdString := uploadId.String()

	if err := s3a.mkdir(s3a.genUploadsFolder(*input.Bucket), uploadIdString, func(entry *filer_pb.Entry) {
		if entry.Extended == nil {
			entry.Extended = make(map[string][]byte)
		}
		entry.Extended["key"] = []byte(*input.Key)
	}); err != nil {
		glog.Errorf("NewMultipartUpload error: %v", err)
		return nil, ErrInternalError
	}

	output = &s3.CreateMultipartUploadOutput{
		Bucket:   input.Bucket,
		Key:      input.Key,
		UploadId: aws.String(uploadIdString),
	}

	return
}

func (s3a *S3ApiServer) completeMultipartUpload(input *s3.CompleteMultipartUploadInput) (output *s3.CompleteMultipartUploadOutput, code ErrorCode) {

	uploadDirectory := s3a.genUploadsFolder(*input.Bucket) + "/" + *input.UploadId

	entries, err := s3a.list(uploadDirectory, "", "", false, 0)
	if err != nil {
		glog.Errorf("completeMultipartUpload %s *s error: %v", *input.Bucket, *input.UploadId, err)
		return nil, ErrNoSuchUpload
	}

	var finalParts []*filer_pb.FileChunk
	var offset int64

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name, ".part") && !entry.IsDirectory {
			for _, chunk := range entry.Chunks {
				finalParts = append(finalParts, &filer_pb.FileChunk{
					FileId: chunk.FileId,
					Offset: offset,
					Size:   chunk.Size,
					Mtime:  chunk.Mtime,
					ETag:   chunk.ETag,
				})
				offset += int64(chunk.Size)
			}
		}
	}

	entryName := filepath.Base(*input.Key)
	dirName := filepath.Dir(*input.Key)
	if dirName == "." {
		dirName = ""
	}
	dirName = fmt.Sprintf("%s/%s/%s", s3a.option.BucketsPath, *input.Bucket, dirName)

	err = s3a.mkFile(dirName, entryName, finalParts)

	if err != nil {
		glog.Errorf("completeMultipartUpload %s/%s error: %v", dirName, entryName, err)
		return nil, ErrInternalError
	}

	output = &s3.CompleteMultipartUploadOutput{
		Bucket: input.Bucket,
		ETag:   aws.String("\"" + filer2.ETag(finalParts) + "\""),
		Key:    input.Key,
	}

	return
}

func (s3a *S3ApiServer) abortMultipartUpload(input *s3.AbortMultipartUploadInput) (output *s3.AbortMultipartUploadOutput, code ErrorCode) {

	exists, err := s3a.exists(s3a.genUploadsFolder(*input.Bucket), *input.UploadId, true)
	if err != nil {
		glog.V(1).Infof("bucket %s abort upload %s: %v", *input.Bucket, *input.UploadId, err)
		return nil, ErrNoSuchUpload
	}
	if exists {
		err = s3a.rm(s3a.genUploadsFolder(*input.Bucket), *input.UploadId, true, true, true)
	}
	if err != nil {
		glog.V(1).Infof("bucket %s remove upload %s: %v", *input.Bucket, *input.UploadId, err)
		return nil, ErrInternalError
	}

	return &s3.AbortMultipartUploadOutput{}, ErrNone
}

func (s3a *S3ApiServer) listMultipartUploads(input *s3.ListMultipartUploadsInput) (output *s3.ListMultipartUploadsOutput, code ErrorCode) {

	output = &s3.ListMultipartUploadsOutput{
		Bucket:       input.Bucket,
		Delimiter:    input.Delimiter,
		EncodingType: input.EncodingType,
		KeyMarker:    input.KeyMarker,
		MaxUploads:   input.MaxUploads,
		Prefix:       input.Prefix,
	}

	entries, err := s3a.list(s3a.genUploadsFolder(*input.Bucket), *input.Prefix, *input.KeyMarker, true, int(*input.MaxUploads))
	if err != nil {
		glog.Errorf("listMultipartUploads %s error: %v", *input.Bucket, err)
		return
	}

	for _, entry := range entries {
		if entry.Extended != nil {
			key := entry.Extended["key"]
			output.Uploads = append(output.Uploads, &s3.MultipartUpload{
				Key:      aws.String(string(key)),
				UploadId: aws.String(entry.Name),
			})
		}
	}
	return
}

func (s3a *S3ApiServer) listObjectParts(input *s3.ListPartsInput) (output *s3.ListPartsOutput, code ErrorCode) {
	output = &s3.ListPartsOutput{
		Bucket:           input.Bucket,
		Key:              input.Key,
		UploadId:         input.UploadId,
		MaxParts:         input.MaxParts,         // the maximum number of parts to return.
		PartNumberMarker: input.PartNumberMarker, // the part number starts after this, exclusive
	}

	entries, err := s3a.list(s3a.genUploadsFolder(*input.Bucket)+"/"+*input.UploadId,
		"", fmt.Sprintf("%04d.part", *input.PartNumberMarker), false, int(*input.MaxParts))
	if err != nil {
		glog.Errorf("listObjectParts %s *s error: %v", *input.Bucket, *input.UploadId, err)
		return nil, ErrNoSuchUpload
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name, ".part") && !entry.IsDirectory {
			partNumberString := entry.Name[:len(entry.Name)-len(".part")]
			partNumber, err := strconv.Atoi(partNumberString)
			if err != nil {
				glog.Errorf("listObjectParts %s *s parse %s: %v", *input.Bucket, *input.UploadId, entry.Name, err)
				continue
			}
			output.Parts = append(output.Parts, &s3.Part{
				PartNumber:   aws.Int64(int64(partNumber)),
				LastModified: aws.Time(time.Unix(entry.Attributes.Mtime, 0)),
				Size:         aws.Int64(int64(filer2.TotalSize(entry.Chunks))),
				ETag:         aws.String("\"" + filer2.ETag(entry.Chunks) + "\""),
			})
		}
	}

	return
}