package proto

// Opcode constants for the connect / re-auth / error and character
// lifecycle messages (M2-T2). Numbers are exact per docs/backend-spec.md
// §6.1–§6.2. Opcodes for later tasks (102–120 gameplay intents,
// 203–215 entity/stat, 211/212/218/220 containers) are NOT defined here.
//
// Directions (no runtime enforcement in this codec):
//
//	C→S: Hello, Reauth, CharacterListRequest, CharacterCreate,
//	     CharacterDelete, EnterWorld, Ack, LeaveWorld
//	S→C: Welcome, ReauthOK, Error,
//	     CharacterListResult, CharacterOp, WorldReady
const (
	// C→S connect / character lifecycle.
	OpcodeHello           uint16 = 100
	OpcodeReauth          uint16 = 101
	OpcodeCharacterList   uint16 = 121
	OpcodeCharacterCreate uint16 = 122
	OpcodeCharacterDelete uint16 = 123
	OpcodeEnterWorld      uint16 = 124
	OpcodeAck             uint16 = 125
	OpcodeLeaveWorld      uint16 = 126

	// S→C connect / character lifecycle.
	OpcodeWelcome             uint16 = 200
	OpcodeReauthOK            uint16 = 201
	OpcodeError               uint16 = 202
	OpcodeCharacterListResult uint16 = 216
	OpcodeCharacterOp         uint16 = 217
	OpcodeWorldReady          uint16 = 219
)
