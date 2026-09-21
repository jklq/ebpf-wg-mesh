package source

import "errors"

var (
	ErrArchiveNotFound  = errors.New("source archive object not found")
	ErrArchiveCorrupt   = errors.New("source archive object is corrupt")
	ErrArchiveTransient = errors.New("source archive storage is temporarily unavailable")
	ErrArchiveTooLarge  = errors.New("source archive exceeds size limit")
)

func IsArchiveNotFound(err error) bool { return errors.Is(err, ErrArchiveNotFound) }

func IsArchiveCorrupt(err error) bool { return errors.Is(err, ErrArchiveCorrupt) }

func IsArchiveTransient(err error) bool { return errors.Is(err, ErrArchiveTransient) }
