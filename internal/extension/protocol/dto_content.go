package protocol

// Content DTOs: reading externalized content refs back from the host. Large
// externalizable payloads travel as content refs; the extension pages them
// through host/content/read.

// ContentReadParams pages one content ref. Offset is a byte position; the
// host answers with at most ContentRefChunkBytes of Base64-decoded data.
type ContentReadParams struct {
	ContentRef string `json:"contentRef" validate:"nonempty"`
	Offset     int64  `json:"offset" validate:"min=0"`
}

// ContentReadResult is one page of content ref data. NextOffset is null at
// end of object; TotalBytes and SHA256 describe the whole object so the
// extension can verify the reassembled bytes.
type ContentReadResult struct {
	ContentRef string          `json:"contentRef" validate:"nonempty"`
	Offset     int64           `json:"offset" validate:"min=0"`
	DataBase64 string          `json:"dataBase64"`
	NextOffset *int64          `json:"nextOffset,omitempty" validate:"min=0"`
	TotalBytes int64           `json:"totalBytes" validate:"min=0"`
	SHA256     string          `json:"sha256" validate:"sha256"`
	Encoding   ContentEncoding `json:"encoding"`
}
