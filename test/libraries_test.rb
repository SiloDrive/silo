require_relative "test_helper"
require "securerandom"

class LibrariesTest < Minitest::Test
  include SiloTestHelper

  def test_create_library
    resp = client.create_library("My Test Library")
    assert resp.ok?, "Create failed: #{resp}"
    assert resp["id"], "Expected id in response"
    assert_equal "My Test Library", resp["name"]

    @test_libraries = [resp["id"]]
  end

  def test_create_library_returns_201
    resp = client.create_library("Status Check")
    assert_equal 201, resp.status

    @test_libraries = [resp["id"]]
  end

  def test_create_library_missing_name
    resp = client.post("/api/silo/v1/libraries", { name: "" })
    assert_equal 400, resp.status
  end

  def test_list_libraries_includes_created
    library_id = create_test_library("Listed Library")

    resp = client.list_libraries
    assert resp.ok?, "List failed: #{resp}"

    libraries = resp.json
    assert_kind_of Array, libraries
    assert libraries.any? { |r| r["id"] == library_id }, "Created library not found in list"
  end

  def test_delete_library
    library_id = create_test_library("To Delete")

    resp = client.delete_library(library_id)
    assert resp.ok?, "Delete failed: #{resp}"

    # Should be gone from the list
    libraries = client.list_libraries.json
    refute libraries.any? { |r| r["id"] == library_id }, "Deleted library still in list"

    # Remove from cleanup list since we already deleted it
    @test_libraries.delete(library_id)
  end

  def test_delete_nonexistent_library
    resp = client.delete_library("00000000-0000-0000-0000-000000000000")
    assert_equal 404, resp.status
  end

  def test_create_and_delete_multiple
    ids = 3.times.map { |i| create_test_library("Batch #{i}") }

    libraries = client.list_libraries.json
    ids.each do |id|
      assert libraries.any? { |r| r["id"] == id }, "Library #{id} not in list"
    end

    ids.each do |id|
      resp = client.delete_library(id)
      assert resp.ok?, "Delete #{id} failed: #{resp}"
    end

    libraries = client.list_libraries.json
    ids.each do |id|
      refute libraries.any? { |r| r["id"] == id }, "Library #{id} still in list after delete"
    end

    @test_libraries = []
  end

  def test_library_has_expected_fields
    library_id = create_test_library("Field Check")

    libraries = client.list_libraries.json
    library = libraries.find { |r| r["id"] == library_id }

    assert library, "Library not found"
    assert_includes library.keys, "id"
    assert_includes library.keys, "name"
    assert_includes library.keys, "update_time"
    assert_includes library.keys, "encrypted"
    assert_equal "Field Check", library["name"]
    assert_equal false, library["encrypted"]
  end
end
