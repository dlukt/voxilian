package proto

// Opcode constants for the connect / re-auth / error, character
// lifecycle (M2-T2), gameplay intent (M2-T3a), and entity/stat (M2-T3b)
// messages. Numbers are exact per docs/backend-spec.md §6.1–§6.3.
// Opcodes 211/212/218/220 (containers/chunks/shops, M2-T3c) are NOT
// defined here.
//
// Directions (no runtime enforcement in this codec):
//
//	C→S: Hello, Reauth, Move–RespawnAck (102–120),
//	     CharacterListRequest, CharacterCreate,
//	     CharacterDelete, EnterWorld, Ack, LeaveWorld
//	S→C: Welcome, ReauthOK, Error, CellSnapshot–Effect (203–210),
//	     TradeResult–Respawn (213–215),
//	     CharacterListResult, CharacterOp, WorldReady
const (
	// C→S connect.
	OpcodeHello  uint16 = 100
	OpcodeReauth uint16 = 101

	// C→S gameplay intents.
	OpcodeMove         uint16 = 102
	OpcodeAttack       uint16 = 103
	OpcodeCast         uint16 = 104
	OpcodeUse          uint16 = 105
	OpcodeGet          uint16 = 106
	OpcodeDrop         uint16 = 107
	OpcodePut          uint16 = 108
	OpcodeGive         uint16 = 109
	OpcodeOffer        uint16 = 110
	OpcodeCounter      uint16 = 111
	OpcodeAccept       uint16 = 112
	OpcodeCancel       uint16 = 113
	OpcodeBuy          uint16 = 114
	OpcodeRest         uint16 = 115
	OpcodeEat          uint16 = 116
	OpcodeSay          uint16 = 117
	OpcodeSayGroup     uint16 = 118
	OpcodeSafetyToggle uint16 = 119
	OpcodeRespawnAck   uint16 = 120

	// C→S character lifecycle.
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
	OpcodeCellSnapshot        uint16 = 203
	OpcodeEntityCreate        uint16 = 204
	OpcodeEntityMove          uint16 = 205
	OpcodeEntityRemove        uint16 = 206
	OpcodeStat                uint16 = 207
	OpcodeStatGroup           uint16 = 208
	OpcodeSaid                uint16 = 209
	OpcodeEffect              uint16 = 210
	OpcodeTradeResult         uint16 = 213
	OpcodeDeath               uint16 = 214
	OpcodeRespawn             uint16 = 215
	OpcodeCharacterListResult uint16 = 216
	OpcodeCharacterOp         uint16 = 217
	OpcodeWorldReady          uint16 = 219
)
