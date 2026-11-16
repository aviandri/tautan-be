package server

// Card-level gates land in a later PR; until then nothing is gated.
func gated(Block) bool { return false }

func (s *Server) unlocked(_ any, _ Block) bool { return true }
