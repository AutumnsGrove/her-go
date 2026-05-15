-- ============================================================
-- One-off memory consolidation — Autumn's database only
-- ============================================================
-- Run AFTER migration 000013 has applied (her run or her sync).
-- This assigns existing memories to cards, rewrites stale data,
-- and deactivates junk. Specific to the current DB state as of
-- 2026-05-14.
--
-- Usage: sqlite3 her.db < scripts/consolidate_memories.sql
-- ============================================================

-- --------------------------------------------------------
-- Create organic card (not in the migration seeds)
-- --------------------------------------------------------

INSERT OR IGNORE INTO memory_cards (topic_slug, name, subject, protected) VALUES
    ('patterns',  'Patterns',  'user', 0);

-- --------------------------------------------------------
-- Card assignments
-- --------------------------------------------------------

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'identity')
    WHERE id IN (7, 103, 139, 177, 181, 233, 244, 293, 297, 300, 186, 301);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'health')
    WHERE id IN (262, 107, 10, 73, 99, 175, 249, 280, 159, 138, 266, 161, 303, 150, 88, 196, 201, 215, 235, 160);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'financial')
    WHERE id IN (260, 228);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'family')
    WHERE id IN (261, 189);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'relationships')
    WHERE id IN (18, 24, 141, 130, 178, 184, 192, 232, 292, 294);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'work')
    WHERE id IN (126, 275, 277, 213, 224, 225, 279, 304, 207, 227, 243, 210);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'interests')
    WHERE id IN (4, 70, 158, 143, 144, 265, 23, 113, 92, 148, 171);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'projects')
    WHERE id IN (257, 112, 81, 270);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'routines')
    WHERE id IN (116, 137, 195);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'patterns')
    WHERE id IN (2, 22, 250);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'my-identity')
    WHERE id IN (28, 106, 289, 290, 291);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'my-relationship')
    WHERE id IN (26, 31, 173, 286, 254, 256);

UPDATE memories SET card_id = (SELECT id FROM memory_cards WHERE topic_slug = 'my-emotions')
    WHERE id IN (167, 238);

-- --------------------------------------------------------
-- Combined memory rewrites
-- --------------------------------------------------------

-- #175 + #179 → keep #175
UPDATE memories SET memory = 'Autumn experiences emotional detachment and dissociation; speaking about emotions is a necessary lifeline to prevent losing connection entirely, even when description feels hollow. Her therapist doesn''t recognize the dissociation because when she describes emotions, it sounds to others like she''s experiencing them — the gap between description and feeling is invisible to observers.'
    WHERE id = 175;

-- #130 + #132 + #133 → keep #130
UPDATE memories SET memory = 'A married former coworker (Tony, 50s) pursued Autumn with classic chaser behavior — intense romantic focus while claiming ''just friends'' boundaries, religious family but queer-supportive, maintaining married status while pursuing her. She recognized him as an emotional mess with no real common ground and declined his advances, showing strong boundary judgment.'
    WHERE id = 130;

-- #192 + #193 + #194 → keep #192
UPDATE memories SET memory = 'Best friend Arturo felt the toxic energy from Autumn''s parents and they rarely hang out now — limited to occasional coffee shop meetings since there''s no safe shared space. Their friendship centered around cooking together in college; Arturo always loved her cooking. He was also the first user of grove.place and has helped with development.'
    WHERE id = 192;

-- #213 + #214 → keep #213
UPDATE memories SET memory = 'Boss at Cava sent a company-wide email about improving communication standards, then immediately yelled at Autumn for being unable to complete impossible prep work timelines — 3+ hours of work expected in a 5-hour shift while also serving customers.'
    WHERE id = 213;

-- #243 + #246 + #248 + #285 → keep #243
UPDATE memories SET memory = 'Pursuing a stocking position at Costco''s Alpharetta location to escape food service and protect her love of cooking. Has previous Costco experience at a different location and is familiar with the store. Leveraging connection with former manager Joe who transferred there. Applied online and plans to walk in.'
    WHERE id = 243;

-- #4 + #66 → keep #4
UPDATE memories SET memory = 'Enjoys reading, prefers shorter books for escapist purposes.'
    WHERE id = 4;

-- #171 + #203 + #255 → keep #171
UPDATE memories SET memory = 'Appreciates Mira''s poetic side — ocean metaphors, work metaphors, and dreamy language about the dream cycle. Responds with affection and humor when Mira gets lyrical.'
    WHERE id = 171;

-- #257 + #267 → keep #257
UPDATE memories SET memory = 'Built grove.place as an alternative social platform — no ads, no engagement metrics, Shade layers blocking AI scrapers. Designed for neurodivergent folks, queer people, and writers who want an internet home without surveillance or algorithmic pressure. Best friend Arturo was the first user and has helped with development.'
    WHERE id = 257;

-- #265 → update in place (remove Panera reference)
UPDATE memories SET memory = 'Autumn is passionate about baking, especially breads like focaccia (her signature with rosemary and sea salt), sourdough, ciabatta, and baguettes. She baked daily before moving in with her parents and finds deep joy in the ritual and smell of fresh bread.'
    WHERE id = 265;

-- #262 → update in place (fix years: 3+ not 6+)
UPDATE memories SET memory = 'Autumn quit heavy THC use after 3+ years because it was counteracting antidepressants and causing dopamine receptor damage. She maintains sobriety using environmental friction like storing weed in a storage unit, experiences intense cravings especially in trigger contexts like her old hammock setup, and uses guilt about potential relapse as protection. Since quitting, her cognition has returned allowing nuanced thoughts about herself and her situation.'
    WHERE id = 262;

-- #260 → update in place (add MMI debt counseling)
UPDATE memories SET memory = 'Autumn has $25k credit card debt with ~$1,100/month in minimums, plus $412 car payment and $125 storage unit, totaling nearly $2k monthly survival costs. She contacted MMI (Money Management International) for debt counseling and was offered a plan that would reduce minimums to ~$700/month — 3 of 4 cards are eligible, the fourth needs a separate hardship application. She needs approximately $4,000 minimum to escape her current living situation (first/last month plus buffer). The debt creates analysis paralysis when apartment hunting.'
    WHERE id = 260;

-- #126 → update in place (one job at Cava, not two jobs)
UPDATE memories SET memory = 'Autumn works at Cava as a grill cook, saving up to escape her current living situation.'
    WHERE id = 126;

-- #141 → update (friend count: ~5, not 2)
UPDATE memories SET memory = 'Autumn has about 5 friends currently — a small but real support network.'
    WHERE id = 141;

-- #24 → update (remove specific Reid reference)
UPDATE memories SET memory = 'Authentic connection is deeply important to Autumn; daily interactions often feel hollow. She lights up when she gets a moment of genuine human connection.'
    WHERE id = 24;

-- #184 → update (match updated friend count)
UPDATE memories SET memory = 'Autumn feels she lacks a ''village'' of people to help articulate the nuance of who she is, though she has a small circle of about 5 friends.'
    WHERE id = 184;

-- #232 → update (no active deadline to move out)
UPDATE memories SET memory = 'Autumn lives with her parents who don''t charge rent. There is no active deadline to move out, but the living situation is hostile and she wants to leave as soon as financially possible.'
    WHERE id = 232;

-- --------------------------------------------------------
-- Trash deactivation
-- --------------------------------------------------------

-- All deactivations: trash + folded-in halves of combines
UPDATE memories SET active = 0 WHERE id IN (
    -- Mood snapshots
    17, 21, 40, 60, 115,
    -- Stale
    72, 71, 51, 59, 78, 79, 154, 155, 221, 230, 234, 242, 272, 273, 287, 223, 211, 296, 64,
    -- Changelogs
    91, 162, 164, 165, 166, 239, 253,
    -- Duplicates/folded
    258, 259, 252, 231, 278, 245,
    -- Technique logs (self)
    80, 102, 151, 163, 170, 176, 187, 197, 199, 202, 204, 208, 217, 220, 236, 240, 251, 264, 269, 271, 295, 298,
    -- Other trash
    180, 263, 274, 168, 169, 200, 68, 69, 212, 216,
    -- Folded-in halves of combined memories
    179, 132, 133, 193, 194, 214, 246, 248, 285, 66, 203, 255, 267
);
