-- One warning per preview, before the reaper destroys it.
--
-- A column rather than deriving it from events. The reaper runs on a one-second
-- tick, so "have we already warned about this one" is asked constantly and must
-- be answerable in the same statement that claims the row -- an events lookup
-- would be a second read, and two reapers (or two ticks) could both pass it
-- before either wrote. The column lets the claim be a single UPDATE ...
-- RETURNING, which is the same shape RedeemJoinToken uses and for the same
-- reason: the check and the mark must not be separable.
ALTER TABLE environments ADD COLUMN expiry_warned_at TIMESTAMPTZ;
