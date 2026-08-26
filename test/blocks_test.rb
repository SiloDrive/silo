require_relative "test_helper"
require "digest"
require "securerandom"

# The chunk surface, end to end against a running server. These cover what the
# in-process Go tests cannot: that the HTTP shapes agree with the
# documentation, that they agree with a client which is not written in Go, and
# that the binary as shipped serves them.
#
# store-v2 changed two things this file used to assume. A chunk id is the
# SHA-256 of its bytes, not the SHA-1, and there is no server-wide block size
# to chunk by -- content-defined chunking puts boundaries where the content
# puts them, so there is no offset for the server to advertise. What did not
# change is the contract that matters: the client picks its own boundaries and
# the server checks only that the bytes hash to the id they arrived under.
class BlocksTest < Minitest::Test
  include SiloTestHelper

  def chunk_id(bytes)
    Digest::SHA256.hexdigest(bytes)
  end

  def test_server_advertises_the_surface
    info = client.server_info
    assert info.ok?
    assert_includes info["features"], "blocks"

    # Deliberately absent. It named the offset a fixed-size chunker cut at --
    # one number, server-wide, that every library shared -- and content-defined
    # chunking killed both halves of that. A client that still reads this field
    # should find nothing rather than a number it would chunk wrongly by.
    assert_nil info["block_size"], "block_size is back; see docs/plans/store-v2.md"
  end

  def test_missing_reports_only_what_is_absent
    library_id = create_test_library
    present = "held content #{SecureRandom.hex(8)}"
    absent = chunk_id("never uploaded #{SecureRandom.hex(8)}")

    assert client.put_block(library_id, chunk_id(present), present).ok?

    resp = client.missing_blocks(library_id, [chunk_id(present), absent])
    assert resp.ok?, resp.to_s
    assert_equal [absent], resp["missing"]
  end

  def test_missing_of_nothing_is_an_empty_array_not_null
    library_id = create_test_library
    resp = client.missing_blocks(library_id, [])
    assert resp.ok?

    # This assertion is the reason the harness is not written in Go. Go
    # unmarshals null and [] into the same nil slice, so a Go test cannot tell
    # them apart; every other language can, and breaks on the first.
    assert_equal [], resp["missing"], "null here breaks every client that is not Go"
  end

  def test_a_chunk_that_does_not_hash_to_its_id_is_refused
    library_id = create_test_library
    wrong_id = chunk_id("one thing")

    resp = client.put_block(library_id, wrong_id, "a different thing")
    assert_equal 400, resp.status, "the store accepted content under an id that is not its hash"

    # And nothing was published under that name, or a later writer of the same
    # id would skip it as already present and serve the wrong bytes forever.
    assert_equal [wrong_id], client.missing_blocks(library_id, [wrong_id])["missing"]
  end

  def test_an_id_of_the_wrong_shape_is_refused_before_the_store_is_touched
    library_id = create_test_library
    content = "some bytes"

    # Forty hex characters: a SHA-1, which is what this surface took before
    # store-v2. The route matches sixty-four, so it is not a chunk id at all
    # and never reaches a handler. Deliberately the old algorithm -- this is a
    # negative case, not a leftover.
    resp = client.put_block(library_id, Digest::SHA1.hexdigest(content), content)
    assert_equal 404, resp.status, "a SHA-1 id reached a handler; the id width is the format"
  end

  def test_a_chunk_already_held_is_accepted_and_says_so
    library_id = create_test_library
    content = "sent twice #{SecureRandom.hex(8)}"

    assert_equal 201, client.put_block(library_id, chunk_id(content), content).status
    assert_equal 200, client.put_block(library_id, chunk_id(content), content).status,
      "re-sending a chunk must succeed: it is what an interrupted upload does on retry"
  end

  # The client chooses its own boundaries. Matching the server's chunker buys
  # better dedup, never correctness -- so a file assembled from chunks the
  # client cut itself has to read back byte for byte.
  def test_a_file_uploaded_as_chunks_reads_back_whole
    library_id = create_test_library
    parts = ["first part ", "second part ", "third and last"]
    content = parts.join
    ids = parts.map { |p| chunk_id(p) }

    missing = client.missing_blocks(library_id, ids)["missing"]
    missing.each do |id|
      assert client.put_block(library_id, id, parts[ids.index(id)]).ok?
    end

    created = client.create_from_blocks(library_id, "/big.bin", ids)
    assert_equal 201, created.status, created.to_s
    assert created["id"], "a write returns the new id so a client can check the transfer end to end"

    got = client.get(client.entries_url(library_id, "/big.bin"))
    assert got.ok?
    assert_equal content.bytesize, got.body.bytesize
    assert_equal content, got.body
  end

  # The property that makes an interrupted upload resumable: asking again is
  # the whole recovery, and the second answer is shorter.
  def test_re_asking_after_uploading_returns_a_shorter_list
    library_id = create_test_library
    parts = 3.times.map { "part #{SecureRandom.hex(8)}" }
    ids = parts.map { |p| chunk_id(p) }

    assert_equal ids, client.missing_blocks(library_id, ids)["missing"]

    client.put_block(library_id, ids[0], parts[0])
    assert_equal ids[1..], client.missing_blocks(library_id, ids)["missing"]

    parts[1..].each_with_index { |p, i| client.put_block(library_id, ids[i + 1], p) }
    assert_equal [], client.missing_blocks(library_id, ids)["missing"]
  end

  def test_creating_from_chunks_that_are_not_there_is_424_and_names_them
    library_id = create_test_library
    absent = chunk_id("never sent #{SecureRandom.hex(8)}")

    resp = client.create_from_blocks(library_id, "/nope.bin", [absent])
    assert_equal 424, resp.status, "a commit naming absent chunks must not read as a client error"
    assert_equal [absent], resp["missing"], "the reply has to say which, or the fix is a re-upload of everything"
  end

  def test_nothing_exists_until_the_last_call
    library_id = create_test_library
    content = "uploaded but never committed #{SecureRandom.hex(8)}"

    assert client.put_block(library_id, chunk_id(content), content).ok?

    listing = client.list_dir(library_id, "/")
    assert listing.ok?
    assert_equal [], listing.json, "uploading chunks created something in the tree"
  end
end
