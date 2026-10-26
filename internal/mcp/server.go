package mcp

type Server struct {
	sessionMgr   *SessionManager
	toolRegistry *ToolRegistry
	resourceURL  string
}

func NewServer(registry *ToolRegistry, resourceURL ...string) *Server {
	resURL := ""
	if len(resourceURL) > 0 {
		resURL = resourceURL[0]
	}
	return &Server{
		sessionMgr:   NewSessionManager(),
		toolRegistry: registry,
		resourceURL:  resURL,
	}
}

func (s *Server) Close() {
	if s.sessionMgr != nil {
		s.sessionMgr.Close()
	}
}
