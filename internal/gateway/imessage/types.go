package imessage

import (
	"encoding/json"
	"time"
)

const (
	chatStyleGroup       = 43
	chatStyleDirect      = 45
	webhookPath          = "/bluebubbles/webhook"
	defaultSendMethod    = "private-api"
	capabilityAttempts   = 5
	capabilityRetryDelay = 500 * time.Millisecond
	typingAfterReadDelay = 150 * time.Millisecond
	messageIndexTTL      = time.Hour
	contactCacheTTL      = 6 * time.Hour
)

type webhookEvent struct {
	Type string         `json:"type"`
	Data webhookMessage `json:"data"`
}

type webhookMessage struct {
	GUID                  string          `json:"guid"`
	Text                  string          `json:"text"`
	IsFromMe              bool            `json:"isFromMe"`
	Handle                messageHandle   `json:"handle"`
	Attachments           []attachment    `json:"attachments"`
	AssociatedMessageType json.RawMessage `json:"associatedMessageType"`
	ReplyToGUID           string          `json:"replyToGuid"`
	ThreadOriginatorGUID  string          `json:"threadOriginatorGuid"`
	Chats                 []messageChat   `json:"chats"`
}

type attachment struct {
	GUID         string `json:"guid"`
	MimeType     string `json:"mimeType"`
	TransferName string `json:"transferName"`
	TotalBytes   int    `json:"totalBytes"`
}

type messageHandle struct {
	Address string `json:"address"`
}

type messageChat struct {
	GUID  string `json:"guid"`
	Style int    `json:"style"`
}

type sendTextRequest struct {
	ChatGUID            string `json:"chatGuid"`
	Message             string `json:"message"`
	Method              string `json:"method,omitempty"`
	SelectedMessageGUID string `json:"selectedMessageGuid,omitempty"`
	PartIndex           int    `json:"partIndex,omitempty"`
	TempGUID            string `json:"tempGuid,omitempty"`
}

type sendTextResponse struct {
	Data struct {
		GUID string `json:"guid"`
	} `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type messageLookupResponse struct {
	Data  messageLookupData `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type messageQueryRequest struct {
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
	With   []string             `json:"with"`
	Where  []messageQueryClause `json:"where"`
}

type messageQueryClause struct {
	Statement string            `json:"statement"`
	Args      map[string]string `json:"args"`
}

type messageQueryResponse struct {
	Data  []messageLookupData `json:"data"`
	Error *struct {
		Error string `json:"error"`
	} `json:"error,omitempty"`
}

type messageLookupData struct {
	GUID        string        `json:"guid"`
	Text        string        `json:"text"`
	IsFromMe    bool          `json:"isFromMe"`
	Handle      messageHandle `json:"handle"`
	Chats       []messageChat `json:"chats"`
	Attachments []attachment  `json:"attachments"`
}

type contactQueryRequest struct {
	Addresses []string `json:"addresses"`
}

type contactQueryResponse struct {
	Data []contactRecord `json:"data"`
}

type contactRecord struct {
	DisplayName string `json:"displayName"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Nickname    string `json:"nickname"`
}

type contactNameCacheEntry struct {
	DisplayName string
	ExpiresAt   time.Time
}

type serverInfoResponse struct {
	Data struct {
		PrivateAPI      bool `json:"private_api"`
		HelperConnected bool `json:"helper_connected"`
	} `json:"data"`
}

type messageContext struct {
	SessionKey  string
	ChatGUID    string
	SenderID    string
	DisplayName string
	Text        string
	Attachments []attachment
	IsFromBot   bool
	CreatedAt   time.Time
}
