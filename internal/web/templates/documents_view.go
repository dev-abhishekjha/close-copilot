package templates

// Document page states. The zero value is the normal page.
const (
	DocsNotConfigured = "not-configured"
	DocsNoCompany     = "no-company"
	DocsNotFound      = "not-found"
	DocsError         = "error"
)

// DocumentsPage is the view of GET /documents. State is one of the Docs*
// constants, or empty for the list.
type DocumentsPage struct {
	User, Role, CSRFToken string
	State                 string
	Tabs                  TabsProps
	Table                 TableProps
}

// DocSectionView is one section of a document; Heading is already defaulted.
type DocSectionView struct {
	Heading, Text string
}

// DocumentPage is the view of GET /documents/{id}.
type DocumentPage struct {
	User, Role, CSRFToken string
	State                 string
	Title                 string
	Facts                 []Fact
	Sections              []DocSectionView
}
