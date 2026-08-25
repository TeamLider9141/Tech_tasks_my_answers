-- Separate database for automated tests so `make test` never touches dev data.
CREATE DATABASE inventory_test OWNER inventory;
