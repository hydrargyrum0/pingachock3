ALTER TABLE nodes ADD COLUMN blocked boolean NOT NULL DEFAULT false;
ALTER TABLE nodes ADD COLUMN platform text NOT NULL DEFAULT '';
