// Package world defines world constants (chunk 16³, cell 32 m, AOI 96 m),
// volume flags, and the WorldSource/CollisionWorld interfaces both world
// modes feed (spec §4, decisions §13.1/§13.5). Full implementation lands
// in M4 (CollisionWorld seam) and M10 (sources, portals, streaming).
package world
