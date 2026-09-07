package delivery

func ValidatePort(port int32) error {
	if port < 1 || port > 65535 {
		return ErrInvalidPort
	}
	return nil
}
